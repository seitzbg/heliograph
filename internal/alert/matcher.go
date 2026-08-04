// Package alert implements SmokePing-style alerting: matchers that inspect a
// window of recent per-round loss/latency samples, an engine that tracks
// firing/resolved state per (target, alert), and notifiers (see codemap 04).
//
// Two matcher styles are provided:
//   - hysteresis matchers (CheckLoss, CheckLatency): raise after X consecutive
//     samples past a threshold, clear after X consecutive back under it;
//   - pattern matchers: a comma-separated sequence of comparisons matched
//     against the tail of the window (SmokePing's shape-matching DSL).
package alert

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Window is the recent sample history for one target, oldest -> newest.
// Loss is a percentage (0..100); RTT is seconds; RTT is NaN for a lost round.
type Window struct {
	Loss []float64
	RTT  []float64
}

// Matcher decides whether an alert should be firing given the window and its
// own previous firing state (which enables hysteresis).
type Matcher interface {
	Length() int                   // trailing samples needed
	Test(w Window, prev bool) bool // new firing state
	Describe() string
}

// ---- hysteresis matchers ----

// CheckLoss raises after X consecutive samples with loss >= L% and clears after
// X consecutive samples with loss < L%.
type CheckLoss struct {
	L float64 // loss threshold, percent
	X int     // consecutive samples
}

func (m CheckLoss) Length() int      { return m.X }
func (m CheckLoss) Describe() string { return fmt.Sprintf("loss >= %g%% for %d samples", m.L, m.X) }
func (m CheckLoss) Test(w Window, prev bool) bool {
	return hysteresis(w.Loss, m.X, prev, func(v float64) bool { return v >= m.L })
}

// CheckLatency raises after X consecutive samples with median RTT >= L seconds
// and clears after X consecutive under it.
type CheckLatency struct {
	L float64 // seconds
	X int
}

func (m CheckLatency) Length() int { return m.X }
func (m CheckLatency) Describe() string {
	return fmt.Sprintf("rtt >= %gms for %d samples", m.L*1000, m.X)
}
func (m CheckLatency) Test(w Window, prev bool) bool {
	return hysteresis(w.RTT, m.X, prev, func(v float64) bool { return !math.IsNaN(v) && v >= m.L })
}

// hysteresis: when not firing, raise iff the last x samples all satisfy pred;
// when firing, clear iff the last x samples all fail pred; otherwise hold.
func hysteresis(series []float64, x int, prev bool, pred func(float64) bool) bool {
	if x <= 0 || len(series) < x {
		return prev
	}
	tail := series[len(series)-x:]
	// An unknown sample (NaN — a fully-lost round has no median) carries no
	// opinion: it must not by itself raise or clear the alert. Hold the previous
	// state so a hard-down host cannot clear its own latency alert (that round is
	// 100% loss, which the loss alert reports instead of latency "recovering").
	for _, v := range tail {
		if math.IsNaN(v) {
			return prev
		}
	}
	if !prev {
		for _, v := range tail {
			if !pred(v) {
				return false
			}
		}
		return true
	}
	for _, v := range tail {
		if pred(v) {
			return true // still bad -> stay firing
		}
	}
	return false
}

// ---- pattern matcher ----

type cmp struct {
	op  string
	val float64
}

// Pattern matches a sequence of comparisons against the tail of the window,
// aligned oldest->newest. Field is "loss" (values are percent) or "rtt" (values
// are seconds). It is a shape match and does not use hysteresis.
type Pattern struct {
	Field string
	Seq   []cmp
	Src   string
}

func (p Pattern) Length() int      { return len(p.Seq) }
func (p Pattern) Describe() string { return p.Field + " pattern " + p.Src }
func (p Pattern) Test(w Window, _ bool) bool {
	series := w.RTT
	if p.Field == "loss" {
		series = w.Loss
	}
	n := len(p.Seq)
	if n == 0 || len(series) < n {
		return false
	}
	tail := series[len(series)-n:]
	for i, c := range p.Seq {
		if !compare(tail[i], c.op, c.val) {
			return false
		}
	}
	return true
}

func compare(a float64, op string, b float64) bool {
	if math.IsNaN(a) {
		return false
	}
	switch op {
	case ">":
		return a > b
	case "<":
		return a < b
	case ">=":
		return a >= b
	case "<=":
		return a <= b
	case "==":
		return a == b
	case "!=":
		return a != b
	}
	return false
}

// ParsePattern compiles "==0%,>20%,>20%" (field "loss", values percent) or
// ">200,>200" (field "rtt", values milliseconds -> seconds).
func ParsePattern(field, src string) (Pattern, error) {
	p := Pattern{Field: field, Src: src}
	for _, tok := range strings.Split(src, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		op := ""
		for _, o := range []string{">=", "<=", "==", "!=", ">", "<"} {
			if strings.HasPrefix(tok, o) {
				op = o
				break
			}
		}
		if op == "" {
			return p, fmt.Errorf("pattern token %q: missing comparison operator", tok)
		}
		rest := strings.TrimSpace(strings.TrimPrefix(tok, op))
		rest = strings.TrimSuffix(rest, "%")
		v, err := strconv.ParseFloat(rest, 64)
		if err != nil {
			return p, fmt.Errorf("pattern token %q: %w", tok, err)
		}
		if field == "rtt" {
			v /= 1000 // ms -> seconds
		}
		p.Seq = append(p.Seq, cmp{op: op, val: v})
	}
	if len(p.Seq) == 0 {
		return p, fmt.Errorf("empty pattern")
	}
	return p, nil
}
