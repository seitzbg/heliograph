package alert

import (
	"math"
	"testing"
	"time"
)

type capture struct{ events []Event }

func (c *capture) Notify(e Event) { c.events = append(c.events, e) }

func statuses(evs []Event) []string {
	out := make([]string, len(evs))
	for i, e := range evs {
		out[i] = e.Status()
	}
	return out
}

func TestEdgeTriggerLifecycle(t *testing.T) {
	cap := &capture{}
	alerts := map[string]*Alert{
		"loss": {Name: "loss", Matcher: CheckLoss{L: 50, X: 2}, EdgeTrigger: true, To: []string{"cap"}},
	}
	e := NewEngine(alerts, map[string]Notifier{"cap": cap})

	when := time.Unix(1_700_000_000, 0)
	seq := []float64{0, 0, 60, 60, 60, 0, 0} // loss %
	var allEvents []Event
	for i, loss := range seq {
		evs := e.Evaluate("t", []string{"loss"}, loss, 0.01, when.Add(time.Duration(i)*time.Minute))
		e.Dispatch(evs)
		allEvents = append(allEvents, evs...)
	}

	// Expect: FIRING when [60,60] first seen (index 3), RESOLVED when back to [0,0] (index 6).
	got := statuses(allEvents)
	if len(got) != 2 || got[0] != "FIRING" || got[1] != "RESOLVED" {
		t.Fatalf("events = %v, want [FIRING RESOLVED]", got)
	}
	if len(cap.events) != 2 {
		t.Errorf("notifier got %d events, want 2", len(cap.events))
	}
	if allEvents[0].When != when.Add(3*time.Minute) {
		t.Errorf("FIRING at %v, want t+3m", allEvents[0].When)
	}
}

func TestNonEdgeEmitsWhileFiring(t *testing.T) {
	alerts := map[string]*Alert{
		"loss": {Name: "loss", Matcher: CheckLoss{L: 50, X: 1}, EdgeTrigger: false, To: []string{"cap"}},
	}
	e := NewEngine(alerts, map[string]Notifier{"cap": &capture{}})
	when := time.Unix(1_700_000_000, 0)

	var got []string
	for i, loss := range []float64{60, 60, 0} {
		evs := e.Evaluate("t", []string{"loss"}, loss, 0.01, when.Add(time.Duration(i)*time.Minute))
		got = append(got, statuses(evs)...)
	}
	// non-edge: fires every active cycle, then one resolved on the clearing edge
	want := []string{"FIRING", "FIRING", "RESOLVED"}
	if len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("events = %v, want %v", got, want)
	}
}

func TestCheckLatencyNoSpuriousResolveOnHardDown(t *testing.T) {
	cap := &capture{}
	alerts := map[string]*Alert{
		"lat": {Name: "lat", Matcher: CheckLatency{L: 0.2, X: 2}, EdgeTrigger: true, To: []string{"cap"}},
	}
	e := NewEngine(alerts, map[string]Notifier{"cap": cap})
	when := time.Unix(1_700_000_000, 0)

	// Two high-latency rounds raise the alert; then the host goes hard down, so
	// each round is fully lost and its median is NaN. The latency alert must hold
	// firing — not emit a spurious RESOLVED while the host is unreachable.
	rtts := []float64{0.3, 0.3, math.NaN(), math.NaN()}
	for i, rtt := range rtts {
		lossPct := 0.0
		if math.IsNaN(rtt) {
			lossPct = 100
		}
		e.Dispatch(e.Evaluate("t", []string{"lat"}, lossPct, rtt, when.Add(time.Duration(i)*time.Minute)))
	}

	got := statuses(cap.events)
	if len(got) != 1 || got[0] != "FIRING" {
		t.Fatalf("events = %v, want exactly [FIRING] (no RESOLVED during hard-down)", got)
	}
}

// A config reload builds a fresh engine. Without inheriting the prior state, an
// already-firing edge-trigger alert re-emits FIRING (notifier spam) and loses its
// sample window. InheritStateFrom must prevent both.
func TestInheritStateAcrossReloadNoSpuriousRefire(t *testing.T) {
	newEng := func(cap Notifier) *Engine {
		return NewEngine(map[string]*Alert{
			"loss": {Name: "loss", Matcher: CheckLoss{L: 50, X: 2}, EdgeTrigger: true, To: []string{"cap"}},
		}, map[string]Notifier{"cap": cap})
	}
	when := time.Unix(1_700_000_000, 0)

	// Drive the first engine into the firing state.
	cap1 := &capture{}
	e1 := newEng(cap1)
	for i, loss := range []float64{60, 60} { // two rounds >= 50% -> FIRING
		e1.Dispatch(e1.Evaluate("t", []string{"loss"}, loss, math.NaN(), when.Add(time.Duration(i)*time.Minute)))
	}
	if got := statuses(cap1.events); len(got) != 1 || got[0] != "FIRING" {
		t.Fatalf("pre-reload events = %v, want [FIRING]", got)
	}

	// Reload: fresh engine inherits state, then the outage continues.
	cap2 := &capture{}
	e2 := newEng(cap2)
	e2.InheritStateFrom(e1, map[string]bool{"t": true})
	e2.Dispatch(e2.Evaluate("t", []string{"loss"}, 60, math.NaN(), when.Add(2*time.Minute)))
	if got := statuses(cap2.events); len(got) != 0 {
		t.Fatalf("post-reload while still down emitted %v, want none (no re-fire)", got)
	}

	// Recovery must still produce exactly one RESOLVED — proving the window
	// history carried over (X=2 low samples clear it).
	for i, loss := range []float64{0, 0} {
		e2.Dispatch(e2.Evaluate("t", []string{"loss"}, loss, 0.01, when.Add(time.Duration(3+i)*time.Minute)))
	}
	if got := statuses(cap2.events); len(got) != 1 || got[0] != "RESOLVED" {
		t.Fatalf("post-reload events = %v, want [RESOLVED]", got)
	}
}

// State for targets/alerts absent from the reloaded config must not be carried.
func TestInheritStateDropsStaleTargets(t *testing.T) {
	mk := func() *Engine {
		return NewEngine(map[string]*Alert{
			"loss": {Name: "loss", Matcher: CheckLoss{L: 50, X: 1}, EdgeTrigger: true, To: []string{"cap"}},
		}, map[string]Notifier{"cap": &capture{}})
	}
	when := time.Unix(1_700_000_000, 0)
	e1 := mk()
	e1.Evaluate("gone", []string{"loss"}, 60, math.NaN(), when) // firing on a target dropped in the new config

	e2 := mk()
	e2.InheritStateFrom(e1, map[string]bool{"kept": true}) // "gone" not in valid set
	if _, ok := e2.state["gone\x00loss"]; ok {
		t.Errorf("state for removed target was carried over")
	}
	if _, ok := e2.win["gone"]; ok {
		t.Errorf("window for removed target was carried over")
	}
}

func TestParseMatcher(t *testing.T) {
	m, err := ParseMatcher("CheckLoss(l=50,x=3)")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	cl, ok := m.(CheckLoss)
	if !ok || cl.L != 50 || cl.X != 3 {
		t.Errorf("got %#v, want CheckLoss{50,3}", m)
	}
	m2, err := ParseMatcher("CheckLatency(l=200,x=2)")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	cl2, ok := m2.(CheckLatency)
	if !ok || cl2.L != 0.2 || cl2.X != 2 { // 200ms -> 0.2s
		t.Errorf("got %#v, want CheckLatency{0.2,2}", m2)
	}
	if _, err := ParseMatcher("Bogus(x=1)"); err == nil {
		t.Errorf("expected error for unknown matcher")
	}
}

func TestParseMatcherRejectsBadArgs(t *testing.T) {
	// A missing or zero l/x silently produces a broken alert: x=0 never raises
	// (a dead alert), and l=0 makes the threshold always-true (fires forever).
	// These must be config errors at parse time, not silent misbehavior.
	bad := []string{
		"CheckLoss(l=50)",     // missing x -> X=0, hysteresis never raises
		"CheckLoss(x=3)",      // missing l -> L=0, "loss >= 0%" always true
		"CheckLoss(l=50,x=0)", // x=0 -> dead alert
		"CheckLoss(l=0,x=3)",  // l=0 -> always fires
		"CheckLoss()",         // nothing set
		"CheckLatency(l=200)", // missing x
		"CheckLatency(x=3)",   // missing l
		"CheckLatency(l=200,x=0)",
		"CheckLatency(l=0,x=3)",
	}
	for _, spec := range bad {
		if _, err := ParseMatcher(spec); err == nil {
			t.Errorf("ParseMatcher(%q): expected a config error, got nil", spec)
		}
	}
}
