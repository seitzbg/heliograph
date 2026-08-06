// Package tcpconnect is a native (no external binary) probe that measures the
// time to establish a TCP connection. Works unprivileged. Registered as
// "TCPConnect". Analogous to SmokePing's TCPPing/AnotherSSH connect timing.
package tcpconnect

import (
	"context"
	"net"
	"strconv"
	"time"

	"smokeping-modern/internal/probe"
)

type tcpProbe struct {
	port     string
	interval time.Duration // min gap between the N connects
}

func init() {
	probe.Register("TCPConnect", "TCP Connect", map[string]probe.VarSpec{
		"port":        {Doc: "TCP port to connect to", Default: "80", Scope: probe.TargetVar, Kind: probe.KindPort},
		"interval_ms": {Doc: "milliseconds between the N connects", Default: "10", Scope: probe.ProbeVar, Kind: probe.KindPositiveInt},
	}, func(cfg map[string]string) (probe.Probe, error) {
		p := &tcpProbe{port: "80", interval: 10 * time.Millisecond}
		if v, ok := cfg["port"]; ok && v != "" {
			p.port = v
		}
		if v, ok := cfg["interval_ms"]; ok && v != "" {
			if ms, err := strconv.Atoi(v); err == nil {
				p.interval = time.Duration(ms) * time.Millisecond
			}
		}
		return p, nil
	})
}

func (p *tcpProbe) Name() string { return "TCPConnect" }

func (p *tcpProbe) Measure(ctx context.Context, t probe.Target, pings int) (probe.Result, error) {
	port := t.Param("port", p.port)
	addr := net.JoinHostPort(t.Host, port)

	var samples []float64
	var d net.Dialer
	for i := 0; i < pings; i++ {
		if err := ctx.Err(); err != nil {
			return probe.Result{Samples: samples}, err
		}
		start := time.Now()
		conn, err := d.DialContext(ctx, "tcp", addr)
		if err == nil {
			samples = append(samples, time.Since(start).Seconds())
			_ = conn.Close()
		}
		// A failed connect (refused/timeout) is a lost ping (absent sample). Either
		// way, honor the inter-attempt delay so a down host isn't flooded with
		// back-to-back SYNs — the delay must not be skipped on failure.
		if p.interval > 0 && i < pings-1 {
			select {
			case <-ctx.Done():
				return probe.Result{Samples: samples}, ctx.Err()
			case <-time.After(p.interval):
			}
		}
	}
	return probe.Result{Samples: samples}, nil
}
