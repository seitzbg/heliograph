// Package irttprobe wraps the irtt(1) client to measure UDP round-trip time,
// one-way delay and jitter against an irtt server. Registered as "IRTT". irtt
// emits per-packet JSON which is parsed for the round-trip times; lost packets
// are simply absent (see codemap 03 — IRTT is the template native UDP probe).
package irttprobe

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os/exec"
	"strconv"
	"syscall"
	"time"

	"smokeping-modern/internal/probe"
)

type irttProbe struct {
	binary   string
	port     string
	interval time.Duration
}

func init() {
	probe.Register("IRTT", "IRTT UDP round-trip", map[string]probe.VarSpec{
		"binary":      {Doc: "path to the irtt binary", Default: "irtt", Scope: probe.ProbeVar},
		"port":        {Doc: "irtt server port", Default: "2112", Scope: probe.TargetVar, Kind: probe.KindPort},
		"interval_ms": {Doc: "send interval in milliseconds", Default: "20", Scope: probe.ProbeVar, Kind: probe.KindInt},
	}, func(cfg map[string]string) (probe.Probe, error) {
		p := &irttProbe{binary: "irtt", port: "2112", interval: 20 * time.Millisecond}
		if v := cfg["binary"]; v != "" {
			p.binary = v
		}
		if v := cfg["port"]; v != "" {
			p.port = v
		}
		if v := cfg["interval_ms"]; v != "" {
			if ms, err := strconv.Atoi(v); err == nil && ms > 0 {
				p.interval = time.Duration(ms) * time.Millisecond
			}
		}
		if _, err := exec.LookPath(p.binary); err != nil {
			return nil, fmt.Errorf("irtt: binary %q not found in PATH: %w", p.binary, err)
		}
		return p, nil
	})
}

func (p *irttProbe) Name() string { return "IRTT" }

func (p *irttProbe) Measure(ctx context.Context, t probe.Target, pings int) (probe.Result, error) {
	addr := net.JoinHostPort(t.Host, t.Param("port", p.port))
	// irtt is duration-based; pick a duration that yields ~pings packets.
	dur := time.Duration(pings-1)*p.interval + p.interval/2
	if dur <= 0 {
		dur = p.interval
	}
	args := []string{
		"client", "-i", p.interval.String(), "-d", dur.String(),
		"-Q", "-o", "-", "--fill=rand", addr,
	}
	cmd := exec.CommandContext(ctx, p.binary, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return nil
	}
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return probe.Result{}, ctx.Err()
		}
		// A run that produced JSON despite a non-zero exit is still usable.
		if s, perr := parseIRTT(stdout.Bytes()); perr == nil && len(s) > 0 {
			return probe.Result{Samples: s}, nil
		}
		return probe.Result{}, fmt.Errorf("irtt: %w", err)
	}
	samples, err := parseIRTT(stdout.Bytes())
	if err != nil {
		return probe.Result{}, err
	}
	return probe.Result{Samples: samples}, nil
}

// parseIRTT extracts non-lost round-trip times (seconds) from irtt JSON output.
// The `lost` field is `false` for received packets and a string otherwise; rtt
// is in nanoseconds.
func parseIRTT(b []byte) ([]float64, error) {
	if len(bytes.TrimSpace(b)) == 0 {
		return nil, fmt.Errorf("irtt: empty output")
	}
	var out struct {
		RoundTrips []struct {
			Lost  json.RawMessage `json:"lost"`
			Delay struct {
				RTT int64 `json:"rtt"`
			} `json:"delay"`
		} `json:"round_trips"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("irtt: parse json: %w", err)
	}
	var samples []float64
	for _, rt := range out.RoundTrips {
		// irtt reports `lost` as the string "false" for received packets (older
		// versions used a boolean); anything else ("true_up", ...) is a loss.
		lost := string(bytes.TrimSpace(rt.Lost))
		if lost != "false" && lost != `"false"` {
			continue
		}
		if rt.Delay.RTT <= 0 {
			continue
		}
		samples = append(samples, float64(rt.Delay.RTT)/1e9)
	}
	return samples, nil
}
