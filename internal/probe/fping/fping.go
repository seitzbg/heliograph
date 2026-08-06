// Package fping wraps the fping(8) binary to measure ICMP echo RTT. Registered
// as "FPing". This is the CLI-wrapper style that most SmokePing probes use
// (see codemap 03 §5). fping is invoked in counting mode (-C n) and its
// per-host RTT list is parsed; "-" entries are lost pings (absent samples).
//
// For the MVP this calls fping once per target (basefork style). A production
// build would batch all of a probe's targets into a single fping call (base
// style) for efficiency — noted in the codemap as a future optimization.
package fping

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"syscall"

	"smokeping-modern/internal/probe"
)

type fpingProbe struct {
	binary     string
	packetsize string
	periodMs   string // -p: milliseconds between successive probes to a host
}

func init() {
	probe.Register("FPing", func(cfg map[string]string) (probe.Probe, error) {
		// Default period 50ms so N probes finish quickly (fping's own default is
		// ~1000ms, which would make pings=10 take ~10s). SmokePing exposes this
		// as mininterval; here it is period_ms.
		p := &fpingProbe{binary: "fping", periodMs: "50"}
		if v, ok := cfg["binary"]; ok && v != "" {
			p.binary = v
		}
		if v, ok := cfg["packetsize"]; ok && v != "" {
			p.packetsize = v
		}
		if v, ok := cfg["period_ms"]; ok && v != "" {
			p.periodMs = v
		}
		if _, err := exec.LookPath(p.binary); err != nil {
			return nil, fmt.Errorf("fping: binary %q not found in PATH: %w", p.binary, err)
		}
		return p, nil
	})
}

func (p *fpingProbe) Name() string     { return "FPing" }
func (p *fpingProbe) Describe() string { return "ICMP Echo Pings" }

func (p *fpingProbe) Schema() map[string]probe.VarSpec {
	return map[string]probe.VarSpec{
		"binary":     {Doc: "path to the fping binary", Default: "fping", Scope: probe.ProbeVar},
		"packetsize": {Doc: "ICMP payload size in bytes", Scope: probe.TargetVar, Kind: probe.KindInt},
		"period_ms":  {Doc: "milliseconds between successive probes to a host (fping -p)", Default: "50", Scope: probe.ProbeVar, Kind: probe.KindInt},
	}
}

func (p *fpingProbe) Measure(ctx context.Context, t probe.Target, pings int) (probe.Result, error) {
	args := []string{"-C", strconv.Itoa(pings), "-q", "-B1", "-r1"}
	if p.periodMs != "" {
		args = append(args, "-p", p.periodMs)
	}
	if ps := t.Param("packetsize", p.packetsize); ps != "" {
		args = append(args, "-b", ps)
	}
	args = append(args, t.Host)

	cmd := exec.CommandContext(ctx, p.binary, args...)
	// Own process group so a timeout kill takes the whole subtree (matches
	// SmokePing basefork's process-group TERM/KILL). Cancel sends SIGKILL to -pgid.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return nil
	}

	var stderr bytes.Buffer
	cmd.Stderr = &stderr // fping -C prints results to stderr by default
	err := cmd.Run()
	// fping exits 1 when a host is unreachable; that is not a probe error, just loss.
	samples := parseCounting(stderr.String(), t.Host)
	if err != nil && len(samples) == 0 {
		if ctx.Err() != nil {
			return probe.Result{}, ctx.Err()
		}
		if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 1 {
			return probe.Result{Samples: nil}, nil // total loss, not an error
		}
		return probe.Result{}, fmt.Errorf("fping: %w (%s)", err, strings.TrimSpace(stderr.String()))
	}
	return probe.Result{Samples: samples}, nil
}

// parseCounting parses fping -C output lines like:
//
//	1.1.1.1 : 0.12 0.15 - 0.13
//
// returning the numeric RTTs converted from milliseconds to seconds; "-" (lost)
// is skipped.
func parseCounting(out, host string) []float64 {
	var samples []float64
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		colon := strings.Index(line, " : ")
		if colon < 0 {
			continue
		}
		// The left side is the address fping resolved/printed; accept any.
		fields := strings.Fields(line[colon+3:])
		for _, f := range fields {
			if f == "-" {
				continue
			}
			if ms, err := strconv.ParseFloat(f, 64); err == nil {
				samples = append(samples, ms/1000.0) // ms -> seconds
			}
		}
	}
	return samples
}
