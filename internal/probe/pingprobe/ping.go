// This file implements the "Ping" probe kind: registration, the config
// factory, and Measure. It ties together the pure helpers in icmpmsg.go
// (build/parse/match) and the socket-opening fallback in listener.go
// (openListener) into the probe.Probe contract.
//
// Measure runs its sender and receiver concurrently (see roundState/
// sendEchoes/receiveLoop below): the receiver is already blocked on
// ReadFrom, waiting, before the first echo goes out, so each reply is timed
// against its own arrival. An earlier version sent all N echoes (with
// interval_ms sleeps between them) and only started reading replies once
// every send had gone out — replies buffer in the socket in the meantime, so
// they all got read back-to-back right after the last send, making
// RTT = readTime - sendTime[seq] track the send schedule (≈ (N-1-seq)*interval)
// instead of the real network latency. A live run caught this: loopback
// (<1ms real RTT) reported a ~100ms median with pings=5/interval_ms=50.
package pingprobe

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/icmp"

	"github.com/seitzbg/heliograph/internal/probe"
)

// pingProbe is the "Ping" kind: a native (no fping binary) ICMP echo prober.
// Fields are set once at construction and never mutated afterward, so a
// single instance is safe for concurrent Measure calls (each call opens its
// own socket and keeps its own local state).
type pingProbe struct {
	interval          time.Duration // gap between successive sends within one Measure round
	mode              string        // "auto" | "unprivileged" | "privileged" — passed to openListener
	defaultPacketsize string        // per-target override via t.Param("packetsize", ...)
}

// maxPacketSize bounds the "packetsize" config var (ICMP payload bytes).
// 65500 sits comfortably under the ~65507-byte ceiling a single IPv4 packet
// can carry as ICMP payload (65535 max IPv4 total length, minus a 20-byte IP
// header and 8-byte ICMP header) and is well within IPv6's much larger
// (jumbogram-capable) limit too, so one constant is a safe bound for both
// families. Without this bound, a schema-valid but huge value (e.g.
// 1073741824) reaches buildEcho's make([]byte, packetsize) on every send —
// an OOM or an impossible-allocation panic. Since scheduler probe goroutines
// do not recover panics on their own (see runOne/safeMeasure), that would
// take down the whole daemon rather than just fail one probe's round.
const maxPacketSize = 65500

// parsePacketSize parses and validates a "packetsize" config value: it must
// be a non-negative integer no larger than maxPacketSize. It is the single
// source of truth for that check, used by the schema Validate hook below,
// the factory (validating the probe-level default), and Measure (validating
// the effective per-target value, which can come from a per-target override
// that bypasses both of the above).
func parsePacketSize(v string) (int, error) {
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("packetsize must be a non-negative integer, got %q", v)
	}
	if n > maxPacketSize {
		return 0, fmt.Errorf("packetsize must be <= %d bytes, got %d", maxPacketSize, n)
	}
	return n, nil
}

func init() {
	probe.Register("Ping", "ICMP Echo (native, no fping)", map[string]probe.VarSpec{
		"packetsize": {
			Doc: fmt.Sprintf("ICMP payload size in bytes (max %d)", maxPacketSize), Default: "56",
			Scope: probe.TargetVar, Kind: probe.KindInt,
			Validate: func(v string) error {
				_, err := parsePacketSize(v)
				return err
			},
		},
		"interval_ms": {
			Doc: "milliseconds between successive echo sends within one round", Default: "50",
			Scope: probe.ProbeVar, Kind: probe.KindPositiveInt,
		},
		"mode": {
			Doc: "socket mode: auto (try unprivileged, fall back to raw), " +
				"unprivileged (datagram only), privileged (raw only)",
			Default: "auto", Scope: probe.ProbeVar,
			Enum: []string{"auto", "unprivileged", "privileged"},
		},
	}, newPingProbe)
}

// newPingProbe is the "Ping" factory. It parses probe-level config
// (interval_ms, mode, packetsize) with the same defaults as the schema, and
// only errors on a value the schema validation wouldn't already reject
// itself (defensive parse fallback below never happens through normal config
// loading, but keeps the factory safe if called directly, e.g. from tests).
func newPingProbe(cfg map[string]string) (probe.Probe, error) {
	p := &pingProbe{
		interval:          50 * time.Millisecond,
		mode:              "auto",
		defaultPacketsize: "56",
	}
	if v, ok := cfg["interval_ms"]; ok && v != "" {
		ms, err := strconv.Atoi(v)
		if err != nil || ms < 1 {
			return nil, fmt.Errorf("pingprobe: interval_ms must be a positive integer, got %q", v)
		}
		p.interval = time.Duration(ms) * time.Millisecond
	}
	if v, ok := cfg["mode"]; ok && v != "" {
		p.mode = v
	}
	if v, ok := cfg["packetsize"]; ok && v != "" {
		if _, err := parsePacketSize(v); err != nil {
			return nil, fmt.Errorf("pingprobe: %w", err)
		}
		p.defaultPacketsize = v
	}
	return p, nil
}

func (p *pingProbe) Name() string { return "Ping" }

// measureCounter backs nextID, which produces the low 16 bits of the ICMP
// echo ID: a process/counter mix so concurrent Measure calls (even across
// probe instances, same process) pick different IDs on a shared raw socket.
// Datagram sockets ignore/rewrite ID themselves, so this only matters for the
// raw-socket "privileged" path.
var measureCounter int32

func nextID() int {
	n := atomic.AddInt32(&measureCounter, 1)
	return (os.Getpid() ^ int(n)) & 0xffff
}

// roundState is the mutable state shared between the sender (Measure's own
// goroutine) and the receiver goroutine for one Measure round. Every field
// is guarded by mu — the sender writes sent[seq], the receiver reads it and
// writes seen[seq]/rtts. Measure allocates a fresh roundState per call, so
// there is no cross-call sharing; each concurrent Measure call gets its own
// conn + roundState (see pingProbe's doc comment on concurrency safety).
type roundState struct {
	mu   sync.Mutex
	sent map[int]time.Time // seq -> send time, written by the sender BEFORE the matching WriteTo
	seen map[int]bool      // seq -> already matched, read/written by the receiver only
	rtts []float64         // completed round-trip times, in seconds
}

// Measure sends `pings` ICMP echo requests to t.Host at p.interval spacing
// and collects replies concurrently, so each RTT reflects real arrival time
// rather than the send schedule (see the package doc comment above).
// Unanswered pings are ordinary loss (absent samples), not an error.
func (p *pingProbe) Measure(ctx context.Context, t probe.Target, pings int) (probe.Result, error) {
	// Validate packetsize before touching the network: a per-target override
	// (t.Param) bypasses both the schema-level check (config load time) and
	// the factory's check of the probe-level default, so this is the last
	// line of defense against an oversized value reaching buildEcho's
	// make([]byte, packetsize) — see maxPacketSize's doc comment.
	packetsize, err := parsePacketSize(t.Param("packetsize", p.defaultPacketsize))
	if err != nil {
		return probe.Result{}, fmt.Errorf("pingprobe: %w", err)
	}

	ip, isV6, err := resolveHost(ctx, t.Host)
	if err != nil {
		return probe.Result{}, fmt.Errorf("pingprobe: resolve %q: %w", t.Host, err)
	}

	conn, kind, err := openListener(icmp.ListenPacket, p.mode, isV6)
	if err != nil {
		return probe.Result{}, err
	}
	defer conn.Close()

	token := make([]byte, 8)
	if _, err := rand.Read(token); err != nil {
		return probe.Result{}, fmt.Errorf("pingprobe: generating token: %w", err)
	}
	id := nextID()

	var dst net.Addr
	if kind == "datagram" {
		dst = &net.UDPAddr{IP: ip}
	} else {
		dst = &net.IPAddr{IP: ip}
	}

	// The read deadline bounds the whole round (send phase + wait for
	// trailing replies), set once up front — before either the sender or the
	// receiver starts — since the receiver is now running throughout the
	// send phase rather than being set up only after it.
	deadline := receiveDeadline(ctx, time.Now(), pings, p.interval)
	_ = conn.SetReadDeadline(deadline)

	// conn.ReadFrom blocks the receiver goroutine until a reply arrives or
	// the deadline above is hit — it does NOT wake up when ctx is merely
	// canceled (net.Conn has no context-aware Read). So a cancellation firing
	// while a read is already in flight would otherwise sit blocked until
	// the full deadline. Fix: a watcher yanks the read deadline to "now" the
	// moment ctx.Done() fires, unblocking ReadFrom immediately. Setting a
	// deadline concurrently with an in-flight read is documented-safe for
	// net.Conn/PacketConn implementations.
	watchDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.SetReadDeadline(time.Now())
		case <-watchDone:
		}
	}()

	st := &roundState{
		sent: make(map[int]time.Time, pings),
		seen: make(map[int]bool, pings),
	}

	// Start the receiver BEFORE sending, so it is already blocked on
	// ReadFrom and reads each reply close to its actual arrival time, rather
	// than after every echo has gone out.
	var recvWG sync.WaitGroup
	recvWG.Add(1)
	go func() {
		defer recvWG.Done()
		receiveLoop(ctx, conn, isV6, token, pings, st)
	}()

	buildErr := sendEchoes(ctx, conn, dst, isV6, id, packetsize, token, pings, p.interval, st)

	recvWG.Wait()
	close(watchDone)

	st.mu.Lock()
	rtts := st.rtts
	st.mu.Unlock()

	if buildErr != nil {
		return probe.Result{Samples: rtts}, buildErr
	}
	// A genuine cancellation (context.Canceled — e.g. process shutdown firing
	// the scheduler's ctx mid-round) is not ordinary loss: surface it as an
	// error so the stored round is tagged via its err column, distinguishing
	// "cut short by cancellation" from genuine loss — matching how
	// tcpconnect.go/dns.go propagate ctx.Err(). An ordinary per-round
	// deadline (context.DeadlineExceeded, or no ctx error at all) keeps the
	// existing behavior: unanswered pings are just loss, no error.
	if errors.Is(ctx.Err(), context.Canceled) {
		return probe.Result{Samples: rtts}, ctx.Err()
	}
	return probe.Result{Samples: rtts}, nil
}

// resolveHost resolves host to a single IP address and reports whether it is
// IPv6. Prefers net.DefaultResolver.LookupIP (works for both names and
// literal addresses) and picks the first result. A successful lookup that
// somehow returns zero addresses is turned into an explicit error rather
// than a nil IP silently propagating (which would otherwise show up as a
// puzzling 100%-loss round instead of a clear resolve failure).
func resolveHost(ctx context.Context, host string) (net.IP, bool, error) {
	ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
	if err != nil {
		return nil, false, err
	}
	if len(ips) == 0 {
		return nil, false, fmt.Errorf("pingprobe: no IP address for %q", host)
	}
	ip := ips[0]
	return ip, ip.To4() == nil, nil
}

// sendEchoes writes `pings` echo requests, p.interval apart, to dst. Each
// seq's send time is recorded in st.sent BEFORE its WriteTo call — not
// after — so the concurrently-running receiver can never observe a reply for
// a seq before that seq's sent[] entry exists: a reply can only exist once
// WriteTo has completed, and WriteTo cannot even begin until sent[seq] is
// already visible under the lock. That ordering is what makes the receiver's
// `sent[seq]` lookup safe without any additional coordination. A failed
// WriteTo just leaves that seq permanently unanswered (ordinary loss) — no
// special-casing needed since nothing will ever arrive to match it. Returns
// a non-nil error only if buildEcho fails (in practice this needs a
// malformed message and shouldn't happen); honors ctx for early abort
// between sends.
func sendEchoes(
	ctx context.Context, conn *icmp.PacketConn, dst net.Addr, isV6 bool, id, packetsize int,
	token []byte, pings int, interval time.Duration, st *roundState,
) error {
	for seq := 0; seq < pings; seq++ {
		if ctx.Err() != nil {
			return nil
		}
		b, err := buildEcho(isV6, id, seq, packetsize, token)
		if err != nil {
			return fmt.Errorf("pingprobe: building echo seq=%d: %w", seq, err)
		}
		st.mu.Lock()
		st.sent[seq] = time.Now()
		st.mu.Unlock()
		_, _ = conn.WriteTo(b, dst) // failure -> this seq just never gets answered
		if seq < pings-1 && interval > 0 {
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(interval):
			}
		}
	}
	return nil
}

// receiveLoop reads replies off conn until every one of the `pings` seqs has
// been matched, ctx is done, or the read deadline (set by the caller before
// starting this goroutine) is reached. Each match's RTT is timed against
// st.sent[seq], recorded by the sender — see sendEchoes's doc comment for why
// that lookup is always populated by the time a genuine reply can arrive.
func receiveLoop(ctx context.Context, conn *icmp.PacketConn, isV6 bool, token []byte, pings int, st *roundState) {
	buf := make([]byte, 1500)
	for {
		st.mu.Lock()
		done := len(st.seen) >= pings
		st.mu.Unlock()
		if done || ctx.Err() != nil {
			return
		}
		n, _, err := conn.ReadFrom(buf)
		if err != nil {
			return // read timeout (deadline, possibly forced by cancellation) or conn closed — stop, keep what we have
		}
		// buf[:n] is handed to ParseMessage with no IP-header stripping: this
		// assumes the kernel already strips the IPv4 header before delivering
		// to a raw ICMP socket, which is true on Linux (verified empirically —
		// see the Task 3 report) but not guaranteed on a BSD-heritage raw
		// socket, which would need IP_STRIPHDR handling here.
		msg, err := icmp.ParseMessage(echoProto(isV6), buf[:n])
		if err != nil {
			continue // not a parseable ICMP message — ignore and keep listening
		}
		st.mu.Lock()
		seq, ok := matchReply(msg, token, pings, st.seen)
		if ok {
			if sendTime, wasSent := st.sent[seq]; wasSent {
				st.rtts = append(st.rtts, time.Since(sendTime).Seconds())
				st.seen[seq] = true
			}
		}
		st.mu.Unlock()
	}
}

// receiveDeadline picks a bound for the whole round: ctx's own Deadline when
// set (the common case — the scheduler always wraps Measure's ctx with its
// own per-round timeout), otherwise an estimate of the send schedule's own
// span (pings sent interval apart, starting at start) plus a fallback
// margin, so Measure still returns promptly when the caller passed a context
// with no deadline at all. Computed once, up front — before either the
// sender or receiver starts — since sends now happen concurrently with
// receives rather than all finishing before the deadline is set.
func receiveDeadline(ctx context.Context, start time.Time, pings int, interval time.Duration) time.Time {
	if dl, ok := ctx.Deadline(); ok {
		return dl
	}
	n := pings
	if n < 1 {
		n = 1
	}
	lastSend := start.Add(time.Duration(n-1) * interval)
	fallback := 4 * interval
	if fallback < time.Second {
		fallback = time.Second
	}
	return lastSend.Add(fallback)
}
