// This file implements the "Ping" probe kind: registration, the config
// factory, and Measure. It ties together the pure helpers in icmpmsg.go
// (build/parse/match) and the socket-opening fallback in listener.go
// (openListener) into the probe.Probe contract.
package pingprobe

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"sync/atomic"
	"time"

	"golang.org/x/net/icmp"

	"smokeping-modern/internal/probe"
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

func init() {
	probe.Register("Ping", "ICMP Echo (native, no fping)", map[string]probe.VarSpec{
		"packetsize": {
			Doc: "ICMP payload size in bytes", Default: "56",
			Scope: probe.TargetVar, Kind: probe.KindInt,
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

// Measure sends `pings` ICMP echo requests to t.Host at p.interval spacing,
// then collects replies until all are accounted for or the deadline/context
// expires. Unanswered pings are ordinary loss (absent samples), not an error.
func (p *pingProbe) Measure(ctx context.Context, t probe.Target, pings int) (probe.Result, error) {
	ip, isV6, err := resolveHost(ctx, t.Host)
	if err != nil {
		return probe.Result{}, fmt.Errorf("pingprobe: resolve %q: %w", t.Host, err)
	}

	conn, kind, err := openListener(icmp.ListenPacket, p.mode, isV6)
	if err != nil {
		return probe.Result{}, err
	}
	defer conn.Close()

	packetsize, err := strconv.Atoi(t.Param("packetsize", p.defaultPacketsize))
	if err != nil || packetsize < 0 {
		return probe.Result{}, fmt.Errorf("pingprobe: invalid packetsize %q", t.Param("packetsize", p.defaultPacketsize))
	}

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

	sent := make(map[int]time.Time, pings)
sendLoop:
	for seq := 0; seq < pings; seq++ {
		if ctx.Err() != nil {
			break sendLoop
		}
		b, err := buildEcho(isV6, id, seq, packetsize, token)
		if err != nil {
			return probe.Result{}, fmt.Errorf("pingprobe: building echo seq=%d: %w", seq, err)
		}
		now := time.Now()
		if _, err := conn.WriteTo(b, dst); err == nil {
			sent[seq] = now
		}
		// else: send failed for this seq — leave it out of `sent`, so it's
		// simply never waited on and counts as loss, matching the "ordinary
		// loss" contract for a receive timeout.
		if seq < pings-1 && p.interval > 0 {
			select {
			case <-ctx.Done():
				break sendLoop
			case <-time.After(p.interval):
			}
		}
	}

	// A genuine cancellation (context.Canceled — e.g. process shutdown firing
	// the scheduler's ctx mid-round) is not ordinary loss: abandon the round
	// and surface it, so the caller doesn't persist a bogus "high loss"
	// sample for a round that was cut short, not actually unanswered. An
	// ordinary per-round deadline (context.DeadlineExceeded, or no ctx error
	// at all) keeps the existing behavior: unanswered pings are just loss.
	if errors.Is(ctx.Err(), context.Canceled) {
		return probe.Result{}, ctx.Err()
	}

	if len(sent) == 0 {
		return probe.Result{}, nil
	}

	rtts := receiveReplies(ctx, conn, isV6, token, pings, sent, p.interval)
	if errors.Is(ctx.Err(), context.Canceled) {
		return probe.Result{Samples: rtts}, ctx.Err()
	}
	return probe.Result{Samples: rtts}, nil
}

// resolveHost resolves host to a single IP address and reports whether it is
// IPv6. Prefers net.DefaultResolver.LookupIP (works for both names and
// literal addresses) and picks the first result.
func resolveHost(ctx context.Context, host string) (net.IP, bool, error) {
	ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
	if err != nil || len(ips) == 0 {
		return nil, false, err
	}
	ip := ips[0]
	return ip, ip.To4() == nil, nil
}

// receiveReplies reads from conn until every sent seq has been matched, the
// receive deadline is reached, or ctx is done. deadline is the later of
// ctx's own deadline (if any) and a fallback cap derived from the last send
// time plus a few multiples of interval, so a caller with no ctx deadline
// still gets a bounded wait.
func receiveReplies(
	ctx context.Context, conn *icmp.PacketConn, isV6 bool, token []byte, pings int,
	sent map[int]time.Time, interval time.Duration,
) []float64 {
	deadline := receiveDeadline(ctx, sent, interval)
	_ = conn.SetReadDeadline(deadline)

	// conn.ReadFrom blocks the goroutine until a reply arrives or the read
	// deadline set above is hit — it does NOT wake up when ctx is merely
	// canceled (net.Conn has no context-aware Read). So a cancellation firing
	// while a read is already in flight would otherwise sit blocked until the
	// full deadline anyway. Fix: a watcher yanks the read deadline to "now"
	// the moment ctx.Done() fires, unblocking ReadFrom immediately. Setting a
	// deadline concurrently with an in-flight read is documented-safe for
	// net.Conn/PacketConn implementations.
	watchDone := make(chan struct{})
	defer close(watchDone)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.SetReadDeadline(time.Now())
		case <-watchDone:
		}
	}()

	var rtts []float64
	seen := make(map[int]bool, len(sent))
	buf := make([]byte, 1500)
	for len(seen) < len(sent) {
		if ctx.Err() != nil {
			break
		}
		n, _, err := conn.ReadFrom(buf)
		if err != nil {
			break // read timeout (deadline, possibly forced by cancellation above) or conn closed — stop, keep what we have
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
		seq, ok := matchReply(msg, token, pings, seen)
		if !ok {
			continue
		}
		startTime, wasSent := sent[seq]
		if !wasSent {
			continue // matched a seq we never actually sent (write failed) — ignore
		}
		rtts = append(rtts, time.Since(startTime).Seconds())
		seen[seq] = true
	}
	return rtts
}

// receiveDeadline picks a read deadline: ctx's own Deadline when set,
// otherwise the last send time plus a few multiples of interval (with a
// floor) so Measure still returns promptly when the caller passed a
// context with no deadline.
func receiveDeadline(ctx context.Context, sent map[int]time.Time, interval time.Duration) time.Time {
	if dl, ok := ctx.Deadline(); ok {
		return dl
	}
	var last time.Time
	for _, t := range sent {
		if t.After(last) {
			last = t
		}
	}
	fallback := 4 * interval
	if fallback < time.Second {
		fallback = time.Second
	}
	return last.Add(fallback)
}
