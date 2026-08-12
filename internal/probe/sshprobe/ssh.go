// Package sshprobe is a native SSH probe (no external binary) that measures the
// time to connect and read the server's SSH identification banner — the
// responsiveness of an SSH service. Registered as "SSH". Analogous to
// SmokePing's AnotherSSH banner timing (see codemap 03).
package sshprobe

import (
	"bufio"
	"context"
	"net"
	"strings"
	"time"

	"github.com/seitzbg/heliograph/internal/probe"
)

type sshProbe struct {
	port     string
	interval time.Duration
}

func init() {
	probe.Register("SSH", "SSH banner", map[string]probe.VarSpec{
		"port": {Doc: "SSH port", Default: "22", Scope: probe.TargetVar, Kind: probe.KindPort},
	}, func(cfg map[string]string) (probe.Probe, error) {
		p := &sshProbe{port: "22", interval: 10 * time.Millisecond}
		if v := cfg["port"]; v != "" {
			p.port = v
		}
		return p, nil
	})
}

func (p *sshProbe) Name() string { return "SSH" }

func (p *sshProbe) Measure(ctx context.Context, t probe.Target, pings int) (probe.Result, error) {
	addr := net.JoinHostPort(t.Host, t.Param("port", p.port))
	var samples []float64
	var d net.Dialer
	for i := 0; i < pings; i++ {
		if err := ctx.Err(); err != nil {
			return probe.Result{Samples: samples}, err
		}
		if rtt, ok := p.once(ctx, &d, addr); ok {
			samples = append(samples, rtt)
		}
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

func (p *sshProbe) once(ctx context.Context, d *net.Dialer, addr string) (float64, bool) {
	start := time.Now()
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return 0, false
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetReadDeadline(dl)
	} else {
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil || !strings.HasPrefix(line, "SSH-") {
		return 0, false // not an SSH banner / timed out => lost
	}
	return time.Since(start).Seconds(), true
}
