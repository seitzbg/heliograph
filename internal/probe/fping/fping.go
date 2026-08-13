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
	"time"

	"github.com/seitzbg/heliograph/internal/probe"
)

const (
	fpingReplyTimeout = time.Second            // default -t: how long to wait for each reply
	fpingMaxPeriod    = 500 * time.Millisecond // default -p cap: spread enough to avoid burst loss without over-spreading a fast link
	fpingMinPeriod    = time.Millisecond
)

// fpingArgs builds the fping command line for `pings` counts against host, SPREADING
// the sends across the round `budget` (the scheduler's per-target deadline) instead of
// bursting them. A tight burst inflates loss on a marginal link — a 50ms burst measured
// ~4x SmokePing's loss on a lossy 2.4GHz link, where SmokePing's spread-out pings match
// the real ~17%. The send interval (-p) and per-reply timeout (-t) are sized to fit N
// pings in the budget; periodOverride/timeoutOverride (0 = derive) tune them, still
// capped to fit so a short step never truncates the fping run into false loss.
func fpingArgs(host string, pings int, budget time.Duration, packetsize string, periodOverride, timeoutOverride time.Duration) []string {
	if pings < 1 {
		pings = 1
	}
	if budget <= 0 {
		budget = time.Duration(pings) * time.Second
	}
	// Per-reply timeout (-t), bounded so it can't consume the whole budget.
	reply := timeoutOverride
	if reply <= 0 {
		reply = fpingReplyTimeout
	}
	if half := budget / 2; reply > half {
		reply = half
	}
	// Send interval (-p): spread the sends across the time left after the reply wait,
	// capped so a fast link's round stays short. An override is honored but still capped
	// to the fair per-ping slot, so a short step can't push the run past the budget.
	slot := (budget - reply) / time.Duration(pings)
	period := fpingMaxPeriod
	if periodOverride > 0 {
		period = periodOverride
	}
	if period > slot {
		period = slot
	}
	if period < fpingMinPeriod {
		period = fpingMinPeriod
	}
	ms := func(d time.Duration) string { return strconv.FormatInt(d.Milliseconds(), 10) }
	args := []string{"-C", strconv.Itoa(pings), "-q", "-B1", "-r1", "-p", ms(period), "-t", ms(reply)}
	if packetsize != "" {
		args = append(args, "-b", packetsize)
	}
	return append(args, host)
}

// parseDurationMs parses a millisecond count into a Duration; "" or invalid/non-positive
// yields 0 ("derive").
func parseDurationMs(s string) time.Duration {
	if ms, err := strconv.Atoi(s); err == nil && ms > 0 {
		return time.Duration(ms) * time.Millisecond
	}
	return 0
}

type fpingProbe struct {
	binary     string
	packetsize string
	periodMs   string // -p override (ms); "" spreads the N pings across the round
	timeoutMs  string // -t override (ms); "" = 1000, auto-reduced to fit the round
}

func init() {
	probe.Register("FPing", "ICMP Echo Pings", map[string]probe.VarSpec{
		"binary":     {Doc: "path to the fping binary", Default: "fping", Scope: probe.ProbeVar},
		"packetsize": {Doc: "ICMP payload size in bytes", Scope: probe.TargetVar, Kind: probe.KindInt},
		"period_ms":  {Doc: "override the send interval in ms (fping -p); by default the N pings are spread across the round so a tight burst doesn't inflate loss on marginal links", Scope: probe.ProbeVar, Kind: probe.KindPositiveInt},
		"timeout_ms": {Doc: "override the per-reply timeout in ms (fping -t); default 1000, auto-reduced to fit the round", Scope: probe.ProbeVar, Kind: probe.KindPositiveInt},
	}, func(cfg map[string]string) (probe.Probe, error) {
		p := &fpingProbe{binary: "fping"}
		if v, ok := cfg["binary"]; ok && v != "" {
			p.binary = v
		}
		if v, ok := cfg["packetsize"]; ok && v != "" {
			p.packetsize = v
		}
		if v, ok := cfg["period_ms"]; ok && v != "" {
			p.periodMs = v
		}
		if v, ok := cfg["timeout_ms"]; ok && v != "" {
			p.timeoutMs = v
		}
		if _, err := exec.LookPath(p.binary); err != nil {
			return nil, fmt.Errorf("fping: binary %q not found in PATH: %w", p.binary, err)
		}
		return p, nil
	})
}

func (p *fpingProbe) Name() string { return "FPing" }

func (p *fpingProbe) Measure(ctx context.Context, t probe.Target, pings int) (probe.Result, error) {
	// The scheduler's per-target deadline is the round budget (timeout*pings, capped
	// by step); spread the N sends across it instead of bursting them.
	var budget time.Duration
	if dl, ok := ctx.Deadline(); ok {
		budget = time.Until(dl)
	}
	args := fpingArgs(t.Host, pings, budget,
		t.Param("packetsize", p.packetsize),
		parseDurationMs(t.Param("period_ms", p.periodMs)),
		parseDurationMs(t.Param("timeout_ms", p.timeoutMs)),
	)

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
	samples := parseCounting(stderr.String(), t.Host)
	if err != nil {
		if ctx.Err() != nil {
			return probe.Result{}, ctx.Err()
		}
		return interpretExit(exitCode(err), err, samples, stderr.String())
	}
	return probe.Result{Samples: samples}, nil
}

// exitCode extracts a process exit code from a *exec.ExitError, or -1 when err is not
// one (e.g. the binary failed to start).
func exitCode(err error) int {
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode()
	}
	return -1
}

// interpretExit maps a failed fping run to a probe result. fping exit 1 means "some
// hosts were unreachable" — ordinary loss — so it returns whatever samples parsed
// (partial or empty). Any other non-zero exit (2 = resolve failure, 3 = bad args,
// 4 = syscall error, -1 = failed to start) is a real probe error even when some
// samples came through, so a real failure isn't silently reported as success.
func interpretExit(code int, runErr error, samples []float64, stderr string) (probe.Result, error) {
	if code == 1 {
		return probe.Result{Samples: samples}, nil
	}
	return probe.Result{}, fmt.Errorf("fping: %w (%s)", runErr, strings.TrimSpace(stderr))
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
