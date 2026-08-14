// Package tcpconnect is a native (no external binary) probe that measures the
// time to establish a TCP connection. Works unprivileged. Registered as
// "TCPConnect". Analogous to SmokePing's TCPPing/AnotherSSH connect timing.
package tcpconnect

import (
	"context"
	"net"
	"strconv"
	"time"

	"github.com/seitzbg/heliograph/internal/probe"
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
	// Fair per-ping share of the round budget so a hung/blackholed host is probed all
	// `pings` times instead of the first connect eating the whole round (correct loss,
	// no worker held for the full step). This is the demo's Unreachable/blackhole case:
	// a dropped SYN otherwise holds one connect for the entire round budget.
	//
	// The inter-attempt sleeps below happen OUTSIDE the per-attempt contexts, so they
	// must be reserved from the budget before it is divided — otherwise the total
	// (N slots + (N-1) delays) overruns the round deadline and a large interval_ms
	// starves the later attempts. PerPingBudgetWithDelay reserves the delays and clamps
	// the interval so it can't consume more than half the budget; interDelay is the
	// (possibly clamped) delay to actually sleep.
	perPing, interDelay := probe.PerPingBudgetWithDelay(ctx, pings, p.interval)
	for i := 0; i < pings; i++ {
		if err := ctx.Err(); err != nil {
			return probe.Result{Samples: samples}, err
		}
		actx, cancel := probe.AttemptContext(ctx, perPing)
		start := time.Now()
		conn, err := d.DialContext(actx, "tcp", addr)
		cancel()
		if err == nil {
			samples = append(samples, time.Since(start).Seconds())
			_ = conn.Close()
		}
		// A failed connect (refused/timeout) is a lost ping (absent sample). Either
		// way, honor the inter-attempt delay so a down host isn't flooded with
		// back-to-back SYNs — the delay must not be skipped on failure.
		if interDelay > 0 && i < pings-1 {
			select {
			case <-ctx.Done():
				return probe.Result{Samples: samples}, ctx.Err()
			case <-time.After(interDelay):
			}
		}
	}
	return probe.Result{Samples: samples}, nil
}
