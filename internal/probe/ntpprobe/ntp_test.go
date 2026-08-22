package ntpprobe

import (
	"context"
	"encoding/binary"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/seitzbg/heliograph/internal/probe"
)

// ntpServer starts an in-process UDP NTP server that replies to every request with a mode-4
// packet at the given stratum, its clock skewed `skew` ahead of the test's clock (so the probe
// should compute an offset of approximately +skew). Returns the server's UDP port.
func ntpServer(t *testing.T, stratum uint8, skew time.Duration) int {
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
				return // closed at teardown
			}
			if n < packetSize {
				continue
			}
			now := time.Now().Add(skew)
			resp := make([]byte, packetSize)
			resp[0] = byte(4<<3) | 0x04 // LI=0, VN=4, Mode=4 (server)
			resp[1] = stratum
			copy(resp[24:32], buf[40:48]) // echo client transmit ts as originate (t1) — a compliant server
			putNTP(resp[32:40], now)      // receive timestamp (t2)
			putNTP(resp[40:48], now)      // transmit timestamp (t3)
			_, _ = pc.WriteTo(resp, addr)
		}
	}()
	return pc.LocalAddr().(*net.UDPAddr).Port
}

// putNTP writes t as an 8-byte NTP timestamp — the inverse of ntpTime.
func putNTP(b []byte, t time.Time) {
	secs := uint32(t.Unix() + ntpUnixEpochDelta)
	frac := uint32((int64(t.Nanosecond()) << 32) / 1e9)
	binary.BigEndian.PutUint32(b[0:4], secs)
	binary.BigEndian.PutUint32(b[4:8], frac)
}

// A synchronized server (stratum 1..15) yields an RTT sample AND records the clock offset +
// stratum for the dashboard. The injected +5s skew must show up as an offset near +5s.
func TestNTPMeasuresRTTAndOffset(t *testing.T) {
	const skew = 5 * time.Second
	port := ntpServer(t, 2, skew)

	p, err := probe.New("NTP", nil)
	if err != nil {
		t.Fatalf("probe.New: %v", err)
	}
	target := probe.Target{Name: "ntp-sync", Host: "127.0.0.1", Params: map[string]string{"port": strconv.Itoa(port)}}

	res, err := p.Measure(context.Background(), target, 3)
	if err != nil {
		t.Fatalf("Measure: %v", err)
	}
	if len(res.Samples) != 3 {
		t.Fatalf("want 3 RTT samples from a responsive server, got %d", len(res.Samples))
	}
	for _, s := range res.Samples {
		if s < 0 || s > 1 {
			t.Errorf("implausible localhost RTT sample %v s", s)
		}
	}

	offSec, stratum, _, ok := LatestFor("ntp-sync")
	if !ok {
		t.Fatal("no offset recorded for a synchronized (stratum 2) server")
	}
	if stratum != 2 {
		t.Errorf("stratum = %d, want 2", stratum)
	}
	if d := time.Duration(offSec * float64(time.Second)); d < skew-200*time.Millisecond || d > skew+200*time.Millisecond {
		t.Errorf("offset = %v, want ~%v (server skewed +%v)", d, skew, skew)
	}
}

// In measure=offset mode the smoke SAMPLES are the clock offsets (signed seconds), not RTTs, so
// the graph plots offset over time. A server skewed +5s must produce samples near +5s.
func TestNTPOffsetModeGraphsOffset(t *testing.T) {
	const skew = 5 * time.Second
	port := ntpServer(t, 2, skew)
	p, _ := probe.New("NTP", nil)
	target := probe.Target{Name: "ntp-offmode", Host: "127.0.0.1", Params: map[string]string{
		"port": strconv.Itoa(port), "measure": "offset",
	}}

	res, err := p.Measure(context.Background(), target, 3)
	if err != nil {
		t.Fatalf("Measure: %v", err)
	}
	if len(res.Samples) != 3 {
		t.Fatalf("want 3 offset samples, got %d", len(res.Samples))
	}
	for _, s := range res.Samples {
		if d := time.Duration(s * float64(time.Second)); d < skew-200*time.Millisecond || d > skew+200*time.Millisecond {
			t.Errorf("offset-mode sample = %v, want ~%v (the clock offset, not RTT)", d, skew)
		}
	}
	if _, _, measure, ok := LatestFor("ntp-offmode"); !ok || measure != "offset" {
		t.Errorf("registry measure = %q (ok=%v), want \"offset\"", measure, ok)
	}
}

// An unsynchronized server (stratum 16) is still reachable, so it yields RTT samples — but its
// offset is meaningless, so nothing is recorded in the offset registry.
func TestNTPUnsyncedServerRecordsNoOffset(t *testing.T) {
	port := ntpServer(t, 16, 3*time.Second)
	p, _ := probe.New("NTP", nil)
	target := probe.Target{Name: "ntp-unsynced", Host: "127.0.0.1", Params: map[string]string{"port": strconv.Itoa(port)}}

	res, err := p.Measure(context.Background(), target, 2)
	if err != nil {
		t.Fatalf("Measure: %v", err)
	}
	if len(res.Samples) != 2 {
		t.Fatalf("stratum-16 server is reachable; want 2 RTT samples, got %d", len(res.Samples))
	}
	if _, _, _, ok := LatestFor("ntp-unsynced"); ok {
		t.Error("stratum-16 (unsynchronized) reply must not record an offset")
	}
}

// TestNTPHungServerMakesBoundedAttempts mirrors the DNS hung-resolver test: against a server that
// receives requests but never answers, the probe must send `pings` bounded requests within the
// round budget and record zero samples — not burn the whole budget on the first request.
func TestNTPHungServerMakesBoundedAttempts(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer pc.Close()

	var mu sync.Mutex
	var reqs int
	go func() {
		buf := make([]byte, 64)
		for {
			if _, _, err := pc.ReadFrom(buf); err != nil {
				return
			}
			mu.Lock()
			reqs++
			mu.Unlock()
			// Never reply.
		}
	}()

	port := pc.LocalAddr().(*net.UDPAddr).Port
	p, _ := probe.New("NTP", nil)
	target := probe.Target{Name: "ntp-hung", Host: "127.0.0.1", Params: map[string]string{"port": strconv.Itoa(port)}}

	const pings = 3
	const budget = 900 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()

	start := time.Now()
	res, _ := p.Measure(ctx, target, pings)
	elapsed := time.Since(start)

	if len(res.Samples) != 0 {
		t.Fatalf("hung server should record no samples, got %d", len(res.Samples))
	}
	mu.Lock()
	got := reqs
	mu.Unlock()
	if got < pings {
		t.Fatalf("want %d bounded requests against the hung server, got %d", pings, got)
	}
	if elapsed > budget+400*time.Millisecond {
		t.Fatalf("overran round budget: elapsed %s > budget %s", elapsed, budget)
	}
}
