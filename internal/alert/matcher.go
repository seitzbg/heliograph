// Package alert implements SmokePing-style alerting: matchers that inspect a
// window of recent per-round loss/latency samples, an engine that tracks
// firing/resolved state per (target, alert), and notifiers (see codemap 04).
//
// Two matcher styles are provided:
//   - hysteresis matchers (CheckLoss, CheckLatency): raise after X consecutive
//     samples past a threshold, clear after X consecutive back under it;
//   - pattern matchers: SmokePing's shape-matching DSL — a comma-separated
//     sequence of per-sample comparisons matched right-anchored against the
//     newest end of the window, with `*` (one arbitrary sample), `*N*` (skip
//     0..N samples), and `==U`/`!=U` (the unknown value: a lost round's missing
//     rtt median). The `S` startup sentinel is intentionally not supported.
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
	// Key is a canonical, stable semantic identity for the matcher: two matchers share a
	// key iff they mean the same thing. A config reload inherits an alert's firing state
	// only when the matcher key is unchanged, so a redefined matcher under the same alert
	// name doesn't carry stale hysteresis into new semantics (CODE_REVIEW #4).
	Key() string
}

// ---- hysteresis matchers ----

// CheckLoss raises after X consecutive samples with loss >= L% and clears after
// X consecutive samples with loss < L%.
type CheckLoss struct {
	L float64 // loss threshold, percent
	X int     // consecutive samples
}

func (m CheckLoss) Length() int      { return m.X }
func (m CheckLoss) Key() string      { return fmt.Sprintf("loss(l=%g,x=%d)", m.L, m.X) }
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
func (m CheckLatency) Key() string { return fmt.Sprintf("rtt(l=%g,x=%d)", m.L, m.X) }
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

// stepKind distinguishes the three token shapes a pattern can hold.
type stepKind int

const (
	stepCmp  stepKind = iota // a comparison against exactly one sample
	stepAny                  // bare `*`: exactly one arbitrary sample
	stepSkip                 // `*N*`: 0..N arbitrary samples
)

// step is one compiled pattern token.
type step struct {
	kind stepKind
	op   string  // stepCmp: comparison operator
	val  float64 // stepCmp: numeric threshold (rtt in seconds, loss in percent)
	isU  bool    // stepCmp: matches the unknown value U (NaN); op is == or !=
	max  int     // stepSkip: maximum samples to skip
}

func (s step) matchValue(v float64) bool {
	if s.isU {
		if s.op == "==" {
			return math.IsNaN(v) // ==U: true iff unknown
		}
		return !math.IsNaN(v) // !=U: true iff known
	}
	return compare(v, s.op, s.val)
}

// Pattern is SmokePing's shape-matching DSL: a sequence of steps matched
// right-anchored against the newest end of the window (oldest->newest), with
// `*N*` skips allowing bounded gaps between hard comparisons. Field is "loss"
// (percent) or "rtt" (seconds). It does not use hysteresis.
type Pattern struct {
	Field string
	Steps []step
	Src   string
}

func (p Pattern) Key() string      { return "pattern:" + p.Field + ":" + p.Src }
func (p Pattern) Describe() string { return p.Field + " pattern " + p.Src }

// Length is the maximum number of trailing samples the pattern can inspect —
// hard/wildcard slots plus every skip's full allowance. It drives how deep the
// engine's per-target window must be, so a `*N*` pattern can actually align (the
// bug the Perl original shipped: too-shallow history made documented patterns
// like `>0%,*12*,>0%` silently never match).
func (p Pattern) Length() int {
	n := 0
	for _, s := range p.Steps {
		switch s.kind {
		case stepSkip:
			n += s.max
		default:
			n++
		}
	}
	if n < 1 {
		n = 1
	}
	return n
}

func (p Pattern) Test(w Window, _ bool) bool {
	series := w.RTT
	if p.Field == "loss" {
		series = w.Loss
	}
	if len(p.Steps) == 0 {
		return false
	}
	// failed memoizes (step index, series position) states already known to be
	// unsatisfiable, so overlapping skip branches don't re-explore them. Without it, a
	// pattern with several `*N*` skips is exponential in the number of skips; with it the
	// work is bounded by len(Steps) × len(series) — a config-authored footgun, not attacker
	// input, but cheap to defuse (CodeRabbit #2).
	failed := make(map[[2]int]bool)
	return matchSteps(p.Steps, len(p.Steps)-1, series, len(series)-1, failed)
}

// matchSteps matches steps[0..si] against the samples ending at index pos,
// walking newest->oldest (steps and samples both consumed from the right, so the
// pattern is anchored to the most recent sample). Samples older than the matched
// run are irrelevant (the pattern floats on the left, bounded by Length). A skip
// tries every allowance 0..max; a comparison/wildcard consumes exactly one. `failed`
// memoizes states that returned false, bounding total work (see Test).
func matchSteps(steps []step, si int, series []float64, pos int, failed map[[2]int]bool) bool {
	if si < 0 {
		return true // every step satisfied
	}
	key := [2]int{si, pos}
	if failed[key] {
		return false // this (step, position) subproblem was already shown unsatisfiable
	}
	s := steps[si]
	if s.kind == stepSkip {
		for k := 0; k <= s.max; k++ {
			if pos-k < -1 {
				break // not enough samples left to skip k
			}
			if matchSteps(steps, si-1, series, pos-k, failed) {
				return true
			}
		}
		failed[key] = true
		return false
	}
	if pos < 0 {
		failed[key] = true
		return false // a hard/wildcard slot with no sample to fill it
	}
	if s.kind == stepAny || s.matchValue(series[pos]) {
		if matchSteps(steps, si-1, series, pos-1, failed) {
			return true
		}
	}
	failed[key] = true
	return false
}

func compare(a float64, op string, b float64) bool {
	if math.IsNaN(a) {
		return false // an unknown sample satisfies no numeric comparison (only ==U)
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

// ParsePattern compiles the shape DSL. Examples:
//
//	==0%,>20%,>20%      loss shape (values are percent)
//	>200,>200           rtt shape (values are milliseconds -> seconds)
//	>0%,*12*,>0%        loss > 0 now and again within the previous 12 samples
//	>200,*,==U          high rtt, any sample, then a fully-lost round
//
// `*` matches one arbitrary sample; `*N*` skips 0..N; `==U`/`!=U` match the
// unknown value (a lost round has no rtt median), valid only on the rtt field.
// The `S` startup sentinel is deliberately unsupported (see parseToken).
func ParsePattern(field, src string) (Pattern, error) {
	p := Pattern{Field: field, Src: src}
	for _, tok := range strings.Split(src, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		st, err := parseToken(field, tok)
		if err != nil {
			return p, err
		}
		p.Steps = append(p.Steps, st)
	}
	if len(p.Steps) == 0 {
		return p, fmt.Errorf("empty pattern")
	}
	// A pattern of only skips/wildcards asserts nothing and would fire on any
	// history — reject it rather than ship a silently always-on alert.
	hard := 0
	for _, s := range p.Steps {
		if s.kind == stepCmp {
			hard++
		}
	}
	if hard == 0 {
		return p, fmt.Errorf("pattern %q has no comparison tokens (it would match any history)", src)
	}
	// Bound the AGGREGATE window depth, not just each skip: many `*N*` tokens each capped at
	// maxAlertX still sum (Length) into a very large per-target buffer and expensive matching.
	if n := p.Length(); n > maxPatternLength {
		return p, fmt.Errorf("pattern %q needs %d samples of history, over the %d cap (fewer/smaller *N* skips)", src, n, maxPatternLength)
	}
	return p, nil
}

func parseToken(field, tok string) (step, error) {
	// `*N*` skip (also catches `**` as an error). Bare `*` (len 1) falls through.
	if len(tok) >= 2 && strings.HasPrefix(tok, "*") && strings.HasSuffix(tok, "*") {
		inner := tok[1 : len(tok)-1]
		n, err := strconv.Atoi(inner)
		// Bound N: a skip's allowance is added into Pattern.Length, which sizes the
		// engine's per-target sample window — so a huge (typo or hostile) N would size a
		// huge buffer. Cap it at the same maxAlertX the hysteresis matchers use.
		if err != nil || n < 0 || n > maxAlertX {
			return step{}, fmt.Errorf("pattern token %q: skip must be *N* with N in [0, %d]", tok, maxAlertX)
		}
		return step{kind: stepSkip, max: n}, nil
	}
	if tok == "*" {
		return step{kind: stepAny}, nil
	}
	op := ""
	for _, o := range []string{">=", "<=", "==", "!=", ">", "<"} {
		if strings.HasPrefix(tok, o) {
			op = o
			break
		}
	}
	if op == "" {
		return step{}, fmt.Errorf("pattern token %q: missing comparison operator", tok)
	}
	rest := strings.TrimSpace(strings.TrimPrefix(tok, op))
	switch rest {
	case "S":
		// The startup sentinel exists in SmokePing only because alert state was
		// in-memory and lost on restart. smokeping-modern keeps a durable store, so
		// "already bad at startup" is answered from real history — the sentinel is
		// dropped by design (blueprint §07) rather than reintroduced as a dead token.
		return step{}, fmt.Errorf("pattern token %q: the S startup sentinel is not supported in smokeping-modern", tok)
	case "U":
		if op != "==" && op != "!=" {
			return step{}, fmt.Errorf("pattern token %q: U is only valid with == or !=", tok)
		}
		if field != "rtt" {
			return step{}, fmt.Errorf("pattern token %q: U (unknown) applies to rtt; loss is always a known percentage", tok)
		}
		return step{kind: stepCmp, op: op, isU: true}, nil
	}
	rest = strings.TrimSuffix(rest, "%")
	v, err := strconv.ParseFloat(rest, 64)
	if err != nil {
		return step{}, fmt.Errorf("pattern token %q: %w", tok, err)
	}
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return step{}, fmt.Errorf("pattern token %q: threshold must be a finite number", tok)
	}
	if field == "loss" {
		if v < 0 || v > 100 {
			return step{}, fmt.Errorf("pattern token %q: loss threshold must be in [0, 100]", tok)
		}
	} else { // rtt (milliseconds)
		if v < 0 {
			return step{}, fmt.Errorf("pattern token %q: rtt threshold must be >= 0", tok)
		}
		v /= 1000 // ms -> seconds
	}
	return step{kind: stepCmp, op: op, val: v}, nil
}
