package alert

import (
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
