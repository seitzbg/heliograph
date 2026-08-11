package alert

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestWebhookFullLossSendsValidBody proves the outage case that matters most: a
// fully-lost round has a NaN median RTT. Go's JSON encoder rejects NaN, so the
// previous marshal produced a nil body and the notifier POSTed an empty request.
// The delivery must carry a valid JSON body with rtt_ms as null.
func TestWebhookFullLossSendsValidBody(t *testing.T) {
	bodies := make(chan []byte, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies <- b
	}))
	defer srv.Close()

	n := NewWebhookNotifier(srv.URL, nil)
	defer n.Close(context.Background())
	n.Notify(Event{Target: "t", Alert: "loss", Firing: true, LossPct: 100, RTTms: math.NaN(), When: time.Unix(1_700_000_000, 0)})

	select {
	case b := <-bodies:
		if len(b) == 0 {
			t.Fatal("webhook body was empty for a full-loss (NaN RTT) event")
		}
		var got map[string]any
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatalf("webhook body is not valid JSON: %v (body=%q)", err, b)
		}
		if got["rtt_ms"] != nil {
			t.Errorf("rtt_ms = %v, want null for a fully-lost round", got["rtt_ms"])
		}
		if got["status"] != "firing" {
			t.Errorf("status = %v, want \"firing\"", got["status"])
		}
	case <-time.After(3 * time.Second):
		t.Fatal("webhook did not deliver within 3s")
	}
}

// A failing endpoint that recovers must be retried, and every attempt for the same
// event must carry the same idempotency key so the receiver can dedupe.
func TestWebhookRetriesThenDelivers(t *testing.T) {
	var attempts atomic.Int32
	var lastIdem atomic.Value
	delivered := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastIdem.Store(r.Header.Get("X-Idempotency-Key"))
		if attempts.Add(1) < 3 { // fail the first two attempts, then succeed
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		select {
		case delivered <- struct{}{}:
		default:
		}
	}))
	defer srv.Close()

	n := NewWebhookNotifierConfig(srv.URL, nil, WebhookConfig{Workers: 1, QueueSize: 8, MaxAttempts: 5, BaseBackoff: time.Millisecond, Timeout: time.Second})
	defer n.Close(context.Background())
	n.Notify(Event{Target: "t", Vantage: "local", Alert: "loss", Firing: true, RTTms: 1, When: time.Unix(1_700_000_000, 0)})

	select {
	case <-delivered:
	case <-time.After(3 * time.Second):
		t.Fatal("webhook never delivered after retries")
	}
	if got := attempts.Load(); got != 3 {
		t.Errorf("attempts = %d, want 3 (two failures then success)", got)
	}
	if key, _ := lastIdem.Load().(string); key != "local|t|loss|firing|1700000000000000000" {
		t.Errorf("idempotency key = %q, want the stable vantage|target|alert|status|when key", key)
	}
}

// A full queue must drop the newest event without blocking Notify (bounding both
// memory and goroutines), and the drop must be counted, not silent.
// A graceful drain must spend its deadline retrying a queued event, not give it a single
// attempt: an edge-triggered FIRING is emitted once, so one failed attempt would lose it (#9).
func TestWebhookRetriesDuringDrain(t *testing.T) {
	attempt1Done := make(chan struct{})
	var attempts atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) == 1 {
			w.WriteHeader(http.StatusInternalServerError) // fail the first attempt
			close(attempt1Done)
			return
		}
		w.WriteHeader(http.StatusNoContent) // succeed the retry
	}))
	defer srv.Close()

	// A 50ms backoff so the retry has not yet fired when we start the drain.
	n := NewWebhookNotifierConfig(srv.URL, nil, WebhookConfig{Workers: 1, QueueSize: 8, MaxAttempts: 5, BaseBackoff: 50 * time.Millisecond, Timeout: time.Second})
	n.Notify(Event{Target: "t", Alert: "loss", Firing: true, RTTms: 1, When: time.Unix(1_700_000_000, 0)})

	<-attempt1Done // the first attempt has failed; the worker is now in backoff
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	n.Close(ctx) // the retry must run within this deadline (old code gave up immediately)

	if d := n.Stats().Delivered; d != 1 {
		t.Errorf("delivered = %d after drain, want 1 (the retry must run within the drain deadline)", d)
	}
}

func TestWebhookDropsWhenQueueFull(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-release }))
	defer srv.Close()

	n := NewWebhookNotifierConfig(srv.URL, nil, WebhookConfig{Workers: 1, QueueSize: 1, MaxAttempts: 1, BaseBackoff: time.Millisecond, Timeout: 5 * time.Second})
	defer n.Close(context.Background())
	defer close(release) // LIFO: unblock the worker before Close drains

	done := make(chan struct{})
	go func() {
		for i := 0; i < 50; i++ { // 1 in flight + 1 queued; the rest must be dropped
			n.Notify(Event{Target: "t", Alert: "loss", Firing: true, RTTms: 1, When: time.Unix(int64(1_700_000_000+i), 0)})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Notify blocked on a full queue; it must drop, non-blocking")
	}
	if st := n.Stats(); st.Dropped == 0 {
		t.Errorf("expected dropped events on a full queue, got dropped=%d", st.Dropped)
	}
}

// Close must drain the queued deliveries (best-effort) within its deadline.
func TestWebhookDrainsOnClose(t *testing.T) {
	var got atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got.Add(1) }))
	defer srv.Close()

	n := NewWebhookNotifierConfig(srv.URL, nil, WebhookConfig{Workers: 2, QueueSize: 16, MaxAttempts: 2, BaseBackoff: time.Millisecond, Timeout: time.Second})
	for i := 0; i < 5; i++ {
		n.Notify(Event{Target: "t", Alert: "loss", Firing: true, RTTms: 1, When: time.Unix(int64(1_700_000_000+i), 0)})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	n.Close(ctx)
	if got.Load() != 5 {
		t.Errorf("drained %d of 5 queued deliveries", got.Load())
	}
}

// Close's deadline must cancel an in-flight request, not just interrupt backoffs — a
// hung endpoint can't keep a delivery (and its worker) running for the full per-attempt
// timeout past the shutdown budget (CODE_REVIEW #6). Signal: the aborted attempt is
// counted (Failed==1) — without cancellation the request would still be in flight and
// nothing would be counted when Close returns.
func TestWebhookCloseCancelsInflightAtDeadline(t *testing.T) {
	release := make(chan struct{})
	startedCh := make(chan struct{})
	var startedOnce atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if startedOnce.CompareAndSwap(false, true) {
			close(startedCh)
		}
		<-release // hold the request open (independent of server-side cancel detection)
	}))
	defer srv.Close()
	defer close(release) // LIFO: unblock the handler before srv.Close waits on the conn

	// Long per-attempt timeout: without deadline-driven cancellation the client's Do
	// would block ~30s, so the delivery would still be in flight when Close returns.
	n := NewWebhookNotifierConfig(srv.URL, nil, WebhookConfig{Workers: 1, QueueSize: 4, MaxAttempts: 3, BaseBackoff: time.Millisecond, Timeout: 30 * time.Second})
	n.Notify(Event{Target: "t", Alert: "loss", Firing: true, RTTms: 1, When: time.Unix(1_700_000_000, 0)})
	select {
	case <-startedCh:
	case <-time.After(3 * time.Second):
		t.Fatal("delivery never reached the server")
	}

	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	n.Close(ctx)
	if el := time.Since(start); el > 5*time.Second {
		t.Fatalf("Close took %v; the in-flight request was not cancelled at the deadline", el)
	}
	if f := n.Stats().Failed; f != 1 {
		t.Errorf("Failed = %d, want 1 (the in-flight delivery must be cancelled + counted, not left hanging)", f)
	}
}

// WriteMetrics must expose the delivery counters so drops/failures are scrapeable.
func TestWebhookWriteMetrics(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	n := NewWebhookNotifierConfig(srv.URL, nil, WebhookConfig{Workers: 1, QueueSize: 8, MaxAttempts: 1, BaseBackoff: time.Millisecond, Timeout: time.Second})
	n.Notify(Event{Target: "t", Alert: "loss", Firing: true, RTTms: 1, When: time.Unix(1_700_000_000, 0)})
	n.Close(context.Background())
	var b strings.Builder
	n.WriteMetrics(&b)
	out := b.String()
	for _, want := range []string{
		"smokeping_webhook_queued_total", "smokeping_webhook_delivered_total",
		"smokeping_webhook_retried_total", "smokeping_webhook_dropped_total",
		"smokeping_webhook_failed_total", "smokeping_webhook_queue_depth",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("WriteMetrics missing %q\n--- got ---\n%s", want, out)
		}
	}
}

type capture struct{ events []Event }

func (c *capture) Notify(e Event) { c.events = append(c.events, e) }

// SeedWindow pre-fills the sample window from stored history so a target that is already
// breaching at startup fires on its FIRST post-boot round, not after X fresh samples (the
// durable-store replacement for the S sentinel).
func TestSeedWindowFiresAlreadyBadTargetOnFirstEvaluate(t *testing.T) {
	m, err := ParseMatcher("CheckLoss(l=50,x=3)") // fire after 3 consecutive rounds >=50% loss
	if err != nil {
		t.Fatal(err)
	}
	alerts := map[string]*Alert{"loss": {Name: "loss", Matcher: m, To: []string{"log"}}}
	cap := &capture{}
	e := NewEngine(alerts, map[string]Notifier{"log": cap})

	// Two already-bad rounds warmed from history (rtt unused by CheckLoss).
	e.SeedWindow("t", "local", []string{"loss"}, []float64{100, 100}, []float64{math.NaN(), math.NaN()})

	// The first real round (also bad) completes 3-in-a-row -> fires now.
	evs := e.Evaluate("t", "local", []string{"loss"}, 100, math.NaN(), time.Unix(1_700_000_000, 0))
	if len(evs) != 1 || !evs[0].Firing {
		t.Fatalf("expected an immediate FIRING from the warmed window, got %+v", evs)
	}

	// Sanity: without warm-start the same first round must NOT fire (needs 3 rounds).
	e2 := NewEngine(alerts, map[string]Notifier{"log": &capture{}})
	if evs := e2.Evaluate("t", "local", []string{"loss"}, 100, math.NaN(), time.Unix(1_700_000_000, 0)); len(evs) != 0 {
		t.Fatalf("cold start must not fire on the first round, got %+v", evs)
	}
}

// Seeding fills only the window, never firing state: a recovering first round must not
// fire, and seeding itself emits nothing.
func TestSeedWindowDoesNotSpuriouslyFire(t *testing.T) {
	m, _ := ParseMatcher("CheckLoss(l=50,x=3)")
	e := NewEngine(map[string]*Alert{"loss": {Name: "loss", Matcher: m, To: []string{"log"}}}, map[string]Notifier{"log": &capture{}})
	e.SeedWindow("t", "local", []string{"loss"}, []float64{100, 100}, []float64{math.NaN(), math.NaN()})
	// First post-boot round is healthy -> the 3-in-a-row is broken -> no fire.
	if evs := e.Evaluate("t", "local", []string{"loss"}, 0, 0.01, time.Unix(1_700_000_000, 0)); len(evs) != 0 {
		t.Fatalf("a recovering round must not fire, got %+v", evs)
	}
}

// The same target measured from two vantages must keep independent windows and firing
// state: one vantage breaching must not fire (or resolve) the other, and each event must
// carry its own vantage (CODE_REVIEW #5 / P2-5).
func TestEvaluateIsolatesVantages(t *testing.T) {
	alerts := map[string]*Alert{
		"loss": {Name: "loss", Matcher: CheckLoss{L: 50, X: 2}, EdgeTrigger: true, To: []string{"cap"}},
	}
	e := NewEngine(alerts, map[string]Notifier{"cap": &capture{}})
	when := time.Unix(1_700_000_000, 0)

	// local sees two lossy rounds -> fires; nyc sees the same two rounds as healthy.
	var localEvents, nycEvents []Event
	for i := 0; i < 2; i++ {
		w := when.Add(time.Duration(i) * time.Minute)
		localEvents = append(localEvents, e.Evaluate("t", "local", []string{"loss"}, 100, math.NaN(), w)...)
		nycEvents = append(nycEvents, e.Evaluate("t", "nyc", []string{"loss"}, 0, 0.01, w)...)
	}
	if len(localEvents) != 1 || !localEvents[0].Firing || localEvents[0].Vantage != "local" {
		t.Fatalf("local should fire once with vantage=local, got %+v", localEvents)
	}
	if len(nycEvents) != 0 {
		t.Fatalf("nyc healthy rounds must not fire while local is down, got %+v", nycEvents)
	}

	// Now nyc goes lossy: it must fire independently, and local staying lossy must not
	// re-fire (edge already delivered) nor resolve.
	var more []Event
	for i := 2; i < 4; i++ {
		w := when.Add(time.Duration(i) * time.Minute)
		more = append(more, e.Evaluate("t", "nyc", []string{"loss"}, 100, math.NaN(), w)...)
		more = append(more, e.Evaluate("t", "local", []string{"loss"}, 100, math.NaN(), w)...)
	}
	if len(more) != 1 || !more[0].Firing || more[0].Vantage != "nyc" {
		t.Fatalf("nyc should fire once independently with vantage=nyc, got %+v", more)
	}
}

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
		evs := e.Evaluate("t", "local", []string{"loss"}, loss, 0.01, when.Add(time.Duration(i)*time.Minute))
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
		evs := e.Evaluate("t", "local", []string{"loss"}, loss, 0.01, when.Add(time.Duration(i)*time.Minute))
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
		e.Dispatch(e.Evaluate("t", "local", []string{"lat"}, lossPct, rtt, when.Add(time.Duration(i)*time.Minute)))
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
		e1.Dispatch(e1.Evaluate("t", "local", []string{"loss"}, loss, math.NaN(), when.Add(time.Duration(i)*time.Minute)))
	}
	if got := statuses(cap1.events); len(got) != 1 || got[0] != "FIRING" {
		t.Fatalf("pre-reload events = %v, want [FIRING]", got)
	}

	// Reload: fresh engine inherits state, then the outage continues.
	cap2 := &capture{}
	e2 := newEng(cap2)
	e2.InheritStateFrom(e1, map[string]bool{"t": true}, nil)
	e2.Dispatch(e2.Evaluate("t", "local", []string{"loss"}, 60, math.NaN(), when.Add(2*time.Minute)))
	if got := statuses(cap2.events); len(got) != 0 {
		t.Fatalf("post-reload while still down emitted %v, want none (no re-fire)", got)
	}

	// Recovery must still produce exactly one RESOLVED — proving the window
	// history carried over (X=2 low samples clear it).
	for i, loss := range []float64{0, 0} {
		e2.Dispatch(e2.Evaluate("t", "local", []string{"loss"}, loss, 0.01, when.Add(time.Duration(3+i)*time.Minute)))
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
	e1.Evaluate("gone", "local", []string{"loss"}, 60, math.NaN(), when) // firing on a target dropped in the new config

	e2 := mk()
	e2.InheritStateFrom(e1, map[string]bool{"kept": true}, nil) // "gone" not in valid set
	if _, ok := e2.state["gone\x00loss"]; ok {
		t.Errorf("state for removed target was carried over")
	}
	if _, ok := e2.win["gone"]; ok {
		t.Errorf("window for removed target was carried over")
	}
}

// Bug A (CODE_REVIEW #4): reusing a target NAME for a different host/probe must not carry the
// old sample window or firing/visible state into the semantically new target. The caller marks
// the target identity-changed via sameTarget[t]=false.
func TestInheritDropsStateOnTargetIdentityChange(t *testing.T) {
	mk := func() *Engine {
		return NewEngine(map[string]*Alert{
			"loss": {Name: "loss", Matcher: CheckLoss{L: 50, X: 1}, EdgeTrigger: true, To: []string{"cap"}},
		}, map[string]Notifier{"cap": &capture{}})
	}
	when := time.Unix(1_700_000_000, 0)
	e1 := mk()
	e1.Dispatch(e1.Evaluate("t", "local", []string{"loss"}, 60, math.NaN(), when)) // firing + visible

	e2 := mk()
	e2.InheritStateFrom(e1, map[string]bool{"t": true}, map[string]bool{"t": false}) // identity changed
	if _, ok := e2.state["local\x00t\x00loss"]; ok {
		t.Error("firing state carried across a target identity change")
	}
	if _, ok := e2.visible["local\x00t\x00loss"]; ok {
		t.Error("visible state carried across a target identity change")
	}
	if _, ok := e2.win["local\x00t"]; ok {
		t.Error("sample window carried across a target identity change")
	}
}

// Bug B (CODE_REVIEW #4): changing a matcher while keeping the alert NAME must not carry the old
// firing/visible state under the new semantics. The target identity is unchanged, so its WINDOW
// still carries (the samples are still valid); only the redefined alert's state resets.
func TestInheritDropsStateOnMatcherChange(t *testing.T) {
	when := time.Unix(1_700_000_000, 0)
	e1 := NewEngine(map[string]*Alert{
		"a": {Name: "a", Matcher: CheckLoss{L: 50, X: 1}, EdgeTrigger: true, To: []string{"cap"}},
		"b": {Name: "b", Matcher: CheckLoss{L: 50, X: 1}, EdgeTrigger: true, To: []string{"cap"}},
	}, map[string]Notifier{"cap": &capture{}})
	e1.Dispatch(e1.Evaluate("t", "local", []string{"a", "b"}, 60, math.NaN(), when)) // both firing + visible

	// Reload: alert "a" keeps its matcher; alert "b" changes its matcher (l=50 -> l=80).
	e2 := NewEngine(map[string]*Alert{
		"a": {Name: "a", Matcher: CheckLoss{L: 50, X: 1}, EdgeTrigger: true, To: []string{"cap"}},
		"b": {Name: "b", Matcher: CheckLoss{L: 80, X: 1}, EdgeTrigger: true, To: []string{"cap"}},
	}, map[string]Notifier{"cap": &capture{}})
	e2.InheritStateFrom(e1, map[string]bool{"t": true}, map[string]bool{"t": true}) // same target

	if _, ok := e2.win["local\x00t"]; !ok {
		t.Error("window must carry across a matcher-only change (same target identity)")
	}
	if _, ok := e2.state["local\x00t\x00a"]; !ok {
		t.Error("unchanged alert 'a' must inherit its firing state")
	}
	if _, ok := e2.state["local\x00t\x00b"]; ok {
		t.Error("redefined alert 'b' must NOT inherit stale firing state")
	}
	if _, ok := e2.visible["local\x00t\x00b"]; ok {
		t.Error("redefined alert 'b' must NOT inherit stale visible state")
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
	got := evSummary(e.Evaluate("t", "local", []string{"crit", "warn"}, 60, math.NaN(), when))
	if len(got) != 1 || got[0] != "crit/FIRING" {
		t.Fatalf("round1 = %v, want [crit/FIRING] (warn inhibited)", got)
	}
	// 20% loss: crit clears, warn still over its threshold -> warn now surfaces.
	got = evSummary(e.Evaluate("t", "local", []string{"crit", "warn"}, 20, math.NaN(), when.Add(time.Minute)))
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
	all = append(all, e.Evaluate("t", "local", []string{"crit", "warn"}, 60, math.NaN(), when)...)
	// Round 2: 0% loss -> both clear. crit resolves (it was delivered); warn must
	// stay silent because its FIRING was never delivered.
	all = append(all, e.Evaluate("t", "local", []string{"crit", "warn"}, 0, 0.01, when.Add(time.Minute))...)

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
	all = append(all, e.Evaluate("t", "local", []string{"crit", "warn"}, 20, 0.01, when)...)
	// Round 2: 60% loss -> crit fires and now inhibits warn (still firing).
	all = append(all, e.Evaluate("t", "local", []string{"crit", "warn"}, 60, math.NaN(), when.Add(time.Minute))...)
	// Round 3: 0% loss -> both clear. warn was delivered in round 1, so it must
	// still emit RESOLVED even though it was inhibited in between.
	all = append(all, e.Evaluate("t", "local", []string{"crit", "warn"}, 0, 0.01, when.Add(2*time.Minute))...)

	got := evSummary(all)
	if !containsStr(got, "warn/FIRING") || !containsStr(got, "warn/RESOLVED") {
		t.Fatalf("events = %v, want warn to both fire and resolve (delivered then inhibited)", got)
	}
}

// An edge-triggered alert whose rising edge was suppressed by inhibition must
// still surface FIRING once the inhibitor clears while it remains active. The
// rising edge only happens once; if it is swallowed while inhibited and delivery
// is keyed to the matcher edge, the alert stays firing forever with no event —
// recipients see the inhibitor resolve and nothing about the outage underneath.
func TestPriorityInhibitionEdgeSurfacesAfterInhibitorClears(t *testing.T) {
	alerts := map[string]*Alert{
		"crit": {Name: "crit", Matcher: CheckLoss{L: 50, X: 1}, Priority: 1, EdgeTrigger: true, To: []string{"log"}},
		"warn": {Name: "warn", Matcher: CheckLoss{L: 10, X: 1}, Priority: 2, EdgeTrigger: true, To: []string{"log"}},
	}
	e := NewEngine(alerts, map[string]Notifier{})
	when := time.Unix(1_700_000_000, 0)

	var all []Event
	// Round 1: 60% loss -> both fire on the same edge; warn is inhibited by crit.
	all = append(all, e.Evaluate("t", "local", []string{"crit", "warn"}, 60, math.NaN(), when)...)
	// Round 2: 60% loss -> both still firing; no new edge for either.
	all = append(all, e.Evaluate("t", "local", []string{"crit", "warn"}, 60, math.NaN(), when.Add(time.Minute))...)
	// Round 3: 20% loss -> crit clears (<50) but warn is still over its 10%
	// threshold. crit's inhibition lifts, so warn must now surface FIRING even
	// though it produced no matcher edge this round.
	all = append(all, e.Evaluate("t", "local", []string{"crit", "warn"}, 20, 0.05, when.Add(2*time.Minute))...)

	got := evSummary(all)
	if !containsStr(got, "crit/RESOLVED") {
		t.Fatalf("events = %v, want crit/RESOLVED after crit clears", got)
	}
	if !containsStr(got, "warn/FIRING") {
		t.Fatalf("events = %v, want warn/FIRING once its inhibitor clears (edge was swallowed while inhibited)", got)
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
	got := evSummary(e.Evaluate("t", "local", []string{"crit", "any"}, 60, math.NaN(), time.Unix(1_700_000_000, 0)))
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
		// Numeric grammar (CODE_REVIEW #7): the arg map must be strict.
		"CheckLoss(l=50,x=3,foo=1)",  // unknown arg -> silently ignored typo
		"CheckLoss(l=50,x=3,l=60)",   // duplicate arg -> last-wins ambiguity
		"CheckLoss(l=NaN,x=3)",       // non-finite threshold
		"CheckLoss(l=Inf,x=3)",       // non-finite threshold
		"CheckLoss(l=50,x=NaN)",      // non-finite count
		"CheckLoss(l=50,x=1.9)",      // fractional x -> truncated to 1 silently
		"CheckLoss(l=50,x=99999999)", // absurd window size
		"CheckLoss(l=150,x=3)",       // loss threshold > 100%
		"CheckLoss(l=-5,x=3)",        // negative loss threshold
		"CheckLatency(l=200,x=1.5)",  // fractional x
		"CheckLatency(l=Inf,x=3)",    // non-finite latency threshold
	}
	for _, spec := range bad {
		if _, err := ParseMatcher(spec); err == nil {
			t.Errorf("ParseMatcher(%q): expected a config error, got nil", spec)
		}
	}
	// Valid specs must still parse (guard against over-tightening).
	for _, spec := range []string{"CheckLoss(l=50,x=3)", "CheckLoss(l=100,x=1)", "CheckLatency(l=200,x=3)"} {
		if _, err := ParseMatcher(spec); err != nil {
			t.Errorf("ParseMatcher(%q): unexpected error %v", spec, err)
		}
	}
}
