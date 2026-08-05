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

// evSummary renders events as "alert/STATUS" for order-independent assertions.
func evSummary(evs []Event) []string {
	out := make([]string, len(evs))
	for i, e := range evs {
		out[i] = e.Alert + "/" + e.Status()
	}
	return out
}

// Priority inhibition: while a higher-priority alert fires on a target, a
// lower-priority one is suppressed; when the inhibitor clears, the lower one
// surfaces (non-edge alerts).
func TestPriorityInhibitionNonEdge(t *testing.T) {
	alerts := map[string]*Alert{
		"crit": {Name: "crit", Matcher: CheckLoss{L: 50, X: 1}, Priority: 1, To: []string{"log"}},
		"warn": {Name: "warn", Matcher: CheckLoss{L: 10, X: 1}, Priority: 2, To: []string{"log"}},
	}
	e := NewEngine(alerts, map[string]Notifier{})
	when := time.Unix(1_700_000_000, 0)

	// 60% loss: both would fire; only the higher-priority crit is emitted.
	got := evSummary(e.Evaluate("t", []string{"crit", "warn"}, 60, math.NaN(), when))
	if len(got) != 1 || got[0] != "crit/FIRING" {
		t.Fatalf("round1 = %v, want [crit/FIRING] (warn inhibited)", got)
	}
	// 20% loss: crit clears, warn still over its threshold -> warn now surfaces.
	got = evSummary(e.Evaluate("t", []string{"crit", "warn"}, 20, math.NaN(), when.Add(time.Minute)))
	if len(got) != 2 || !containsStr(got, "crit/RESOLVED") || !containsStr(got, "warn/FIRING") {
		t.Fatalf("round2 = %v, want crit/RESOLVED + warn/FIRING", got)
	}
}

// An edge-triggered alert whose FIRING is inhibited must NOT later emit a lone
// RESOLVED — recipients would see a resolution for something they never saw fire.
func TestPriorityInhibitionNoOrphanResolved(t *testing.T) {
	alerts := map[string]*Alert{
		"crit": {Name: "crit", Matcher: CheckLoss{L: 50, X: 1}, Priority: 1, EdgeTrigger: true, To: []string{"log"}},
		"warn": {Name: "warn", Matcher: CheckLoss{L: 10, X: 1}, Priority: 2, EdgeTrigger: true, To: []string{"log"}},
	}
	e := NewEngine(alerts, map[string]Notifier{})
	when := time.Unix(1_700_000_000, 0)

	var all []Event
	// Round 1: 60% loss -> both fire; warn (lower priority) is inhibited on its
	// rising edge, so only crit is delivered.
	all = append(all, e.Evaluate("t", []string{"crit", "warn"}, 60, math.NaN(), when)...)
	// Round 2: 0% loss -> both clear. crit resolves (it was delivered); warn must
	// stay silent because its FIRING was never delivered.
	all = append(all, e.Evaluate("t", []string{"crit", "warn"}, 0, 0.01, when.Add(time.Minute))...)

	for _, ev := range all {
		if ev.Alert == "warn" {
			t.Errorf("warn emitted %s but its FIRING was inhibited — orphan event", ev.Status())
		}
	}
	got := evSummary(all)
	if len(got) != 2 || got[0] != "crit/FIRING" || got[1] != "crit/RESOLVED" {
		t.Fatalf("events = %v, want [crit/FIRING crit/RESOLVED] only", got)
	}
}

// An alert delivered (visible) before a higher-priority alert starts inhibiting
// it must still receive its RESOLVED — no dangling firing.
func TestPriorityInhibitionStillResolvesDeliveredAlert(t *testing.T) {
	alerts := map[string]*Alert{
		"crit": {Name: "crit", Matcher: CheckLoss{L: 50, X: 1}, Priority: 1, EdgeTrigger: true, To: []string{"log"}},
		"warn": {Name: "warn", Matcher: CheckLoss{L: 10, X: 1}, Priority: 2, EdgeTrigger: true, To: []string{"log"}},
	}
	e := NewEngine(alerts, map[string]Notifier{})
	when := time.Unix(1_700_000_000, 0)

	var all []Event
	// Round 1: 20% loss -> only warn fires (crit needs >=50). warn is delivered.
	all = append(all, e.Evaluate("t", []string{"crit", "warn"}, 20, 0.01, when)...)
	// Round 2: 60% loss -> crit fires and now inhibits warn (still firing).
	all = append(all, e.Evaluate("t", []string{"crit", "warn"}, 60, math.NaN(), when.Add(time.Minute))...)
	// Round 3: 0% loss -> both clear. warn was delivered in round 1, so it must
	// still emit RESOLVED even though it was inhibited in between.
	all = append(all, e.Evaluate("t", []string{"crit", "warn"}, 0, 0.01, when.Add(2*time.Minute))...)

	got := evSummary(all)
	if !containsStr(got, "warn/FIRING") || !containsStr(got, "warn/RESOLVED") {
		t.Fatalf("events = %v, want warn to both fire and resolve (delivered then inhibited)", got)
	}
}

// An alert with no priority (0) is never inhibited, even when a higher-priority
// alert is firing on the same target.
func TestPriorityUnsetAlwaysNotifies(t *testing.T) {
	alerts := map[string]*Alert{
		"crit": {Name: "crit", Matcher: CheckLoss{L: 50, X: 1}, Priority: 1, EdgeTrigger: true, To: []string{"log"}},
		"any":  {Name: "any", Matcher: CheckLoss{L: 10, X: 1}, Priority: 0, EdgeTrigger: true, To: []string{"log"}},
	}
	e := NewEngine(alerts, map[string]Notifier{})
	got := evSummary(e.Evaluate("t", []string{"crit", "any"}, 60, math.NaN(), time.Unix(1_700_000_000, 0)))
	if len(got) != 2 || !containsStr(got, "crit/FIRING") || !containsStr(got, "any/FIRING") {
		t.Fatalf("got %v, want both crit/FIRING and any/FIRING (unset priority not inhibited)", got)
	}
}

// Dispatch delivers to the alert's To notifiers plus the per-target alertee,
// each recipient exactly once even when it appears in both lists.
func TestDispatchAlertee(t *testing.T) {
	primary, extra := &capture{}, &capture{}
	alerts := map[string]*Alert{"a": {Name: "a", To: []string{"primary", "extra"}}}
	e := NewEngine(alerts, map[string]Notifier{"primary": primary, "extra": extra})
	ev := Event{Target: "t", Alert: "a", Firing: true}
	// "extra" is in both To and the alertee list -> must still fire only once.
	e.Dispatch([]Event{ev}, "extra")
	if len(primary.events) != 1 {
		t.Errorf("primary got %d events, want 1", len(primary.events))
	}
	if len(extra.events) != 1 {
		t.Errorf("extra (in To and alertee) got %d events, want 1 (deduped)", len(extra.events))
	}
}

func containsStr(s []string, want string) bool {
	for _, v := range s {
		if v == want {
			return true
		}
	}
	return false
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
