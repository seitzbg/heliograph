package ntpprobe

import (
	"context"
	"net"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/seitzbg/heliograph/internal/probe"
)

// rawNTPServer starts an in-process UDP server that builds each mode-4 reply by calling
// reply(req) — letting a test craft stratum, timestamps, and origin echo per request. A nil
// return means "do not answer" (a lost ping). reqs (if non-nil) counts received requests; it is
// atomic so a test can read it while the server goroutine runs.
func rawNTPServer(t *testing.T, reqs *atomic.Int64, reply func(req []byte) []byte) int {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { pc.Close() })
	go func() {
		buf := make([]byte, 64)
		for {
			n, addr, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			if n < packetSize {
				continue
			}
			if reqs != nil {
				reqs.Add(1)
			}
			if resp := reply(buf[:packetSize]); resp != nil {
				_, _ = pc.WriteTo(resp, addr)
			}
		}
	}()
	return pc.LocalAddr().(*net.UDPAddr).Port
}

// compliantReply builds a well-formed mode-4 reply at the given stratum that echoes the
// client's transmit timestamp as the originate timestamp and sets sane t2<=t3.
func compliantReply(stratum uint8, skew time.Duration) func(req []byte) []byte {
	return func(req []byte) []byte {
		now := time.Now().Add(skew)
		resp := make([]byte, packetSize)
		resp[0] = byte(4<<3) | 0x04
		resp[1] = stratum
		copy(resp[24:32], req[40:48]) // echo originate
		putNTP(resp[32:40], now)      // t2
		putNTP(resp[40:48], now)      // t3
		return resp
	}
}

// The probe result carries the metric kind (rtt vs offset) it measured, derived from config —
// present even when the round is fully lost, so downstream storage/serve can tag the row.
func TestNTPResultCarriesMetricKind(t *testing.T) {
	port := rawNTPServer(t, nil, compliantReply(2, time.Second))
	p, _ := probe.New("NTP", nil)

	off := probe.Target{Name: "k-off", Host: "127.0.0.1", Params: map[string]string{"port": strconv.Itoa(port), "measure": "offset"}}
	if r, _ := p.Measure(context.Background(), off, 1); r.Kind != probe.MetricOffset {
		t.Errorf("offset mode: Result.Kind = %q, want %q", r.Kind, probe.MetricOffset)
	}
	rtt := probe.Target{Name: "k-rtt", Host: "127.0.0.1", Params: map[string]string{"port": strconv.Itoa(port)}}
	if r, _ := p.Measure(context.Background(), rtt, 1); r.Kind != probe.MetricRTT {
		t.Errorf("rtt mode: Result.Kind = %q, want %q", r.Kind, probe.MetricRTT)
	}
	// Fully-lost offset round (nothing answers) must still report the configured kind.
	lost := probe.Target{Name: "k-lost", Host: "127.0.0.1", Params: map[string]string{"port": "1", "measure": "offset"}}
	if r, _ := p.Measure(context.Background(), lost, 1); r.Kind != probe.MetricOffset {
		t.Errorf("lost offset round: Result.Kind = %q, want %q (kind is config-derived, not data-derived)", r.Kind, probe.MetricOffset)
	}
}

// M3: a synchronized reply records the offset/stratum stat; if the SAME target later answers
// unsynchronized (stratum 16), the stale "good clock" stat must be invalidated, not left visible.
func TestNTPStatInvalidatedOnUnsync(t *testing.T) {
	syncedPort := rawNTPServer(t, nil, compliantReply(2, time.Second))
	p, _ := probe.New("NTP", nil)
	synced := probe.Target{Name: "desync", Host: "127.0.0.1", Params: map[string]string{"port": strconv.Itoa(syncedPort)}}
	if _, err := p.Measure(context.Background(), synced, 1); err != nil {
		t.Fatalf("Measure(synced): %v", err)
	}
	if _, _, _, ok := LatestFor("desync", ""); !ok {
		t.Fatal("expected an offset stat after a synchronized (stratum 2) reply")
	}
	// Same target name, now answering unsynchronized (stratum 16).
	unsyncedPort := rawNTPServer(t, nil, compliantReply(16, time.Second))
	unsynced := probe.Target{Name: "desync", Host: "127.0.0.1", Params: map[string]string{"port": strconv.Itoa(unsyncedPort)}}
	if _, err := p.Measure(context.Background(), unsynced, 1); err != nil {
		t.Fatalf("Measure(unsynced): %v", err)
	}
	if _, _, _, ok := LatestFor("desync", ""); ok {
		t.Error("stale offset stat must be invalidated after an unsynchronized reply")
	}
}

// M7 (round 2): a leap-indicator alarm (LI=3) means the server's own clock is unsynchronized, so
// even a low-stratum, well-timestamped reply is not a trustworthy time source — no offset sample or
// stat, though RTT reachability still counts.
func TestNTPLeapAlarmRejectsOffset(t *testing.T) {
	leapAlarm := func(req []byte) []byte {
		now := time.Now().Add(time.Second)
		resp := make([]byte, packetSize)
		resp[0] = byte(3<<6) | byte(4<<3) | 0x04 // LI=3 (alarm), VN=4, Mode=4 (server)
		resp[1] = 2                              // stratum 2 — otherwise "synchronized"
		copy(resp[24:32], req[40:48])            // echo origin
		putNTP(resp[32:40], now)
		putNTP(resp[40:48], now)
		return resp
	}
	port := rawNTPServer(t, nil, leapAlarm)
	p, _ := probe.New("NTP", nil)

	off := probe.Target{Name: "leap-off", Host: "127.0.0.1", Params: map[string]string{"port": strconv.Itoa(port), "measure": "offset"}}
	res, _ := p.Measure(context.Background(), off, 2)
	if len(res.Samples) != 0 {
		t.Errorf("LI=3 offset-mode samples = %d, want 0 (clock unsynchronized)", len(res.Samples))
	}
	if _, _, _, ok := LatestFor("leap-off", ""); ok {
		t.Error("LI=3 reply must not record an offset stat")
	}
	rtt := probe.Target{Name: "leap-rtt", Host: "127.0.0.1", Params: map[string]string{"port": strconv.Itoa(port)}}
	res2, _ := p.Measure(context.Background(), rtt, 2)
	if len(res2.Samples) != 2 {
		t.Errorf("LI=3 RTT-mode samples = %d, want 2 (still reachable)", len(res2.Samples))
	}
}

// M3 (round 2): after a server stops answering entirely (no reply), a round that produces no
// synchronized sample must clear the stale offset/stratum stat — not leave a "good clock" reading
// visible through the outage.
func TestNTPStatClearedAfterNoReplyRound(t *testing.T) {
	goodPort := rawNTPServer(t, nil, compliantReply(2, time.Second))
	p, _ := probe.New("NTP", nil)
	good := probe.Target{Name: "gone", Host: "127.0.0.1", Params: map[string]string{"port": strconv.Itoa(goodPort)}}
	if _, err := p.Measure(context.Background(), good, 1); err != nil {
		t.Fatalf("Measure(good): %v", err)
	}
	if _, _, _, ok := LatestFor("gone", ""); !ok {
		t.Fatal("expected a stat after a synchronized reply")
	}
	// Same target now points at a closed port: every ping is a no-reply (connection refused).
	gone := probe.Target{Name: "gone", Host: "127.0.0.1", Params: map[string]string{"port": "1"}}
	if _, err := p.Measure(context.Background(), gone, 2); err != nil {
		t.Fatalf("Measure(gone): %v", err)
	}
	if _, _, _, ok := LatestFor("gone", ""); ok {
		t.Error("stale offset/stratum stat must clear after a no-reply round")
	}
}

// M7: in offset mode, an unsynchronized reply (stratum 0 KoD or 16+) has no usable offset and
// must NOT produce an offset sample — it is a lost sample this round.
func TestNTPOffsetModeRejectsUnsynchronized(t *testing.T) {
	for _, stratum := range []uint8{0, 16} {
		port := rawNTPServer(t, nil, compliantReply(stratum, 3*time.Second))
		p, _ := probe.New("NTP", nil)
		target := probe.Target{Name: "unsync", Host: "127.0.0.1", Params: map[string]string{
			"port": strconv.Itoa(port), "measure": "offset",
		}}
		res, err := p.Measure(context.Background(), target, 2)
		if err != nil {
			t.Fatalf("stratum %d: Measure: %v", stratum, err)
		}
		if len(res.Samples) != 0 {
			t.Errorf("stratum %d: offset-mode samples = %d, want 0 (unsynchronized)", stratum, len(res.Samples))
		}
	}
}

// M1: a stratum-0 Kiss-o'-Death reply means "stop querying me" — the probe must back off and
// send no more requests this round, and must not count the KoD as a usable sample.
func TestNTPKissODeathStopsRound(t *testing.T) {
	var reqs atomic.Int64
	port := rawNTPServer(t, &reqs, compliantReply(0, 0))
	p, _ := probe.New("NTP", nil)
	target := probe.Target{Name: "kod", Host: "127.0.0.1", Params: map[string]string{"port": strconv.Itoa(port)}}

	res, err := p.Measure(context.Background(), target, 10)
	if err != nil {
		t.Fatalf("Measure: %v", err)
	}
	if len(res.Samples) != 0 {
		t.Errorf("KoD must not be a usable sample; got %d samples", len(res.Samples))
	}
	// Give the server a beat to record any (erroneous) extra requests.
	time.Sleep(20 * time.Millisecond)
	if got := reqs.Load(); got != 1 {
		t.Errorf("after a KoD the probe must stop; server saw %d requests, want 1", got)
	}
}

// M1: a multi-sample round against a responsive server must be paced (interval_ms), not sent as
// a tight back-to-back burst.
func TestNTPPacesMultiSampleRound(t *testing.T) {
	port := rawNTPServer(t, nil, compliantReply(2, 0))
	p, _ := probe.New("NTP", map[string]string{"interval_ms": "40"})
	target := probe.Target{Name: "paced", Host: "127.0.0.1", Params: map[string]string{"port": strconv.Itoa(port)}}

	start := time.Now()
	res, err := p.Measure(context.Background(), target, 4)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Measure: %v", err)
	}
	if len(res.Samples) != 4 {
		t.Fatalf("want 4 samples from a responsive server, got %d", len(res.Samples))
	}
	if min := 3 * 40 * time.Millisecond; elapsed < min {
		t.Errorf("round finished in %v; want >= %v (3 gaps of 40ms) — round was not paced", elapsed, min)
	}
}

// L1: publishing an offset requires the reply's originate timestamp to match the request's
// transmit timestamp. A server that does not echo it is still reachable (RTT), but yields no
// offset — so offset mode records nothing and the stat registry stays empty.
func TestNTPOffsetRequiresOriginEcho(t *testing.T) {
	// Reply is well-formed EXCEPT it never echoes the originate timestamp (bytes 24:32 stay zero).
	noEcho := func(req []byte) []byte {
		now := time.Now().Add(2 * time.Second)
		resp := make([]byte, packetSize)
		resp[0] = byte(4<<3) | 0x04
		resp[1] = 2
		putNTP(resp[32:40], now)
		putNTP(resp[40:48], now)
		return resp
	}
	port := rawNTPServer(t, nil, noEcho)

	// Offset mode: no valid offset -> no samples.
	p, _ := probe.New("NTP", nil)
	off := probe.Target{Name: "noecho-off", Host: "127.0.0.1", Params: map[string]string{"port": strconv.Itoa(port), "measure": "offset"}}
	res, _ := p.Measure(context.Background(), off, 2)
	if len(res.Samples) != 0 {
		t.Errorf("wrong-origin offset-mode samples = %d, want 0", len(res.Samples))
	}
	if _, _, _, ok := LatestFor("noecho-off", ""); ok {
		t.Error("wrong-origin reply must not record an offset stat")
	}

	// RTT mode: the host answered, so it is still reachable.
	rtt := probe.Target{Name: "noecho-rtt", Host: "127.0.0.1", Params: map[string]string{"port": strconv.Itoa(port)}}
	res2, _ := p.Measure(context.Background(), rtt, 2)
	if len(res2.Samples) != 2 {
		t.Errorf("wrong-origin RTT-mode samples = %d, want 2 (still reachable)", len(res2.Samples))
	}
	if _, _, _, ok := LatestFor("noecho-rtt", ""); ok {
		t.Error("wrong-origin reply must not record an offset stat in RTT mode either")
	}
}

// L1: a reply whose transmit timestamp precedes its receive timestamp (t3<t2) is nonsensical and
// must not publish an offset.
func TestNTPOffsetRejectsReversedTimestamps(t *testing.T) {
	reversed := func(req []byte) []byte {
		now := time.Now()
		resp := make([]byte, packetSize)
		resp[0] = byte(4<<3) | 0x04
		resp[1] = 2
		copy(resp[24:32], req[40:48])              // echo origin (so only the ordering is wrong)
		putNTP(resp[32:40], now)                   // t2 = now
		putNTP(resp[40:48], now.Add(-time.Second)) // t3 = now-1s  (t3 < t2)
		return resp
	}
	port := rawNTPServer(t, nil, reversed)
	p, _ := probe.New("NTP", nil)
	target := probe.Target{Name: "reversed", Host: "127.0.0.1", Params: map[string]string{"port": strconv.Itoa(port), "measure": "offset"}}
	res, _ := p.Measure(context.Background(), target, 2)
	if len(res.Samples) != 0 {
		t.Errorf("reversed-timestamp offset-mode samples = %d, want 0", len(res.Samples))
	}
}
