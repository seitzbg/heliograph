package dns

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/seitzbg/heliograph/internal/probe"
)

// captureServer starts an in-process UDP DNS server that records the query type
// of the most recent request and returns a minimal reply. Returns its address.
func captureServer(t *testing.T, gotType *uint16, mu *sync.Mutex) *dns.Server {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &dns.Server{PacketConn: pc, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, r *dns.Msg) {
		if len(r.Question) > 0 {
			mu.Lock()
			*gotType = r.Question[0].Qtype
			mu.Unlock()
		}
		m := new(dns.Msg)
		m.SetReply(r)
		_ = w.WriteMsg(m)
	})}
	go func() { _ = srv.ActivateAndServe() }()
	return srv
}

// The per-target `recordtype` param (declared TargetVar in the schema) must drive
// the actual query type — the schema advertised a capability the probe ignored.
func TestDNSPerTargetRecordType(t *testing.T) {
	var gotType uint16
	var mu sync.Mutex
	srv := captureServer(t, &gotType, &mu)
	defer srv.Shutdown()
	time.Sleep(20 * time.Millisecond) // let the server bind

	addr := srv.PacketConn.LocalAddr().(*net.UDPAddr)
	p, err := probe.New("DNS", nil) // probe-level default recordtype = A
	if err != nil {
		t.Fatalf("probe.New: %v", err)
	}

	// Per-target override to AAAA must be the type the server sees.
	target := probe.Target{Name: "r", Host: "127.0.0.1", Params: map[string]string{
		"port": itoa(addr.Port), "recordtype": "AAAA", "lookup": "example.com",
	}}
	if _, err := p.Measure(context.Background(), target, 1); err != nil {
		t.Fatalf("Measure: %v", err)
	}
	mu.Lock()
	got := gotType
	mu.Unlock()
	if got != dns.TypeAAAA {
		t.Errorf("server saw query type %d, want AAAA (%d) — per-target recordtype ignored", got, dns.TypeAAAA)
	}

	// With no per-target override, it falls back to the probe default (A).
	mu.Lock()
	gotType = 0
	mu.Unlock()
	target.Params["recordtype"] = ""
	if _, err := p.Measure(context.Background(), target, 1); err != nil {
		t.Fatalf("Measure default: %v", err)
	}
	mu.Lock()
	got = gotType
	mu.Unlock()
	if got != dns.TypeA {
		t.Errorf("server saw query type %d, want A (%d) as the default", got, dns.TypeA)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
