package alert

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Alert is a named, compiled alert attached to targets.
type Alert struct {
	Name        string
	Matcher     Matcher
	EdgeTrigger bool
	Comment     string
	To          []string // notifier names
	// Priority inhibits noisier alerts: among alerts on the same target, when one
	// with a priority is firing, lower-priority ones (higher number) are suppressed
	// that round. 1 is the highest priority; 0 means "no priority" — never
	// suppressed and never suppresses others. (SmokePing semantics; the modern
	// analogue is Alertmanager inhibit_rules.)
	Priority int
}

// Event is emitted when an alert changes/holds state for a target.
type Event struct {
	Target  string    `json:"target"`
	Vantage string    `json:"vantage,omitempty"` // measuring vantage; "local" is the hub's own
	Alert   string    `json:"alert"`
	Comment string    `json:"comment"`
	Firing  bool      `json:"firing"` // true = raised, false = resolved
	LossPct float64   `json:"loss_pct"`
	RTTms   float64   `json:"rtt_ms"`
	When    time.Time `json:"when"`
}

func (e Event) Status() string {
	if e.Firing {
		return "FIRING"
	}
	return "RESOLVED"
}

// Notifier delivers an event.
type Notifier interface{ Notify(Event) }

// winKey and stKey namespace engine state by vantage, so one target measured from
// several vantages keeps independent sample windows and firing state: a local outage
// and an nyc outage on the same target never share hysteresis, and their events never
// dedupe each other (CODE_REVIEW #5 / P2-5). The hub's own measurements use "local".
func winKey(vantage, target string) string { return vantage + "\x00" + target }
func stKey(vantage, target, name string) string {
	return vantage + "\x00" + target + "\x00" + name
}

// splitStateKey extracts (target, name) from a vantage\x00target\x00name state key,
// discarding the vantage prefix — the two fields InheritStateFrom filters on.
func splitStateKey(key string) (target, name string, ok bool) {
	parts := strings.SplitN(key, "\x00", 3)
	if len(parts) != 3 {
		return "", "", false
	}
	return parts[1], parts[2], true
}

// Engine tracks per-(vantage,target) sample windows and per-(vantage,target,alert)
// firing state.
type Engine struct {
	mu        sync.Mutex
	alerts    map[string]*Alert
	notifiers map[string]Notifier
	win       map[string]*Window
	state     map[string]bool // matcher firing state (drives hysteresis)
	// visible is the recipient's view: true iff a FIRING was actually delivered
	// (not inhibited) and not yet closed. It gates RESOLVED so a suppressed alert
	// never emits an orphan RESOLVED, and a delivered one always gets its close-out.
	visible map[string]bool
	// warnedUnknown debounces the "unknown notifier" warning so a persistent typo
	// logs once, not once per dispatched event. Guarded by its own mutex so it
	// never contends with Evaluate's mu.
	warnMu        sync.Mutex
	warnedUnknown map[string]bool
}

func NewEngine(alerts map[string]*Alert, notifiers map[string]Notifier) *Engine {
	return &Engine{
		alerts:    alerts,
		notifiers: notifiers,
		win:       map[string]*Window{},
		state:     map[string]bool{},
		visible:   map[string]bool{},
	}
}

// ReloadIdentity carries the per-target and per-alert identity a config reload computes so
// InheritStateFrom knows what may survive the swap. Every map is keyed by the NEW runtime's
// names. A nil map field is permissive ("unconstrained" / all-true) — used by tests and the
// boot path — so a caller supplies only the dimensions it distinguishes.
type ReloadIdentity struct {
	// ValidTarget[t]: target t still exists (has alerts) in the new config. A target absent
	// here has its window and all alert state dropped.
	ValidTarget map[string]bool
	// SameTarget[t]: t's measurement identity (host/probe/params/...) is unchanged. If it
	// changed, t's window and alert state are not inherited (t is seeded from history instead).
	SameTarget map[string]bool
	// Attached[t][name]: alert `name` is attached to target t in the NEW config. State for an
	// alert not currently attached to t is never inherited — so detaching an alert and later
	// re-attaching it can't resurrect firing/visible state it held before it was detached
	// (CODE_REVIEW #3a).
	Attached map[string]map[string]bool
	// SameAlertee[t]: t's per-target `alertee` recipients are unchanged. Together with the
	// alert's own To recipients (compared inside the engine), this gates whether the delivered
	// FIRING (visible) bit may be inherited: a recipient-set change must not let a newly added
	// recipient miss an open FIRING, nor receive a RESOLVED for a firing it never saw
	// (CODE_REVIEW #3b).
	SameAlertee map[string]bool
}

// InheritStateFrom seeds this engine with the firing state and sample windows of
// a previous engine, so a config reload (which builds a fresh engine) does not
// reset alert hysteresis. Without this a reload would (a) re-emit FIRING for an
// alert that is already firing, spamming notifiers, and (b) drop the windowed
// loss/RTT history, so a currently-firing alert reads as not-firing until X new
// samples re-accumulate — silently resolving mid-outage, then re-raising later.
//
// Inheritance is gated by identity so a reload that redefines a target, detaches/redefines an
// alert, or changes an alert's delivery can't carry stale state into the new definition
// (CODE_REVIEW #4, #3). For each key:
//   - window (per target): inherited only if the target is present (ValidTarget) with an
//     unchanged measurement identity (SameTarget).
//   - matcher firing state (per target+alert): the above, AND the alert is still attached to
//     that target (Attached), AND its matcher's Key() is unchanged.
//   - delivered/visible bit (per target+alert): the matcher-state conditions above, AND the
//     delivery recipients are unchanged (the alert's To — compared here — and the target's
//     alertee, via SameAlertee). Matcher state may survive a recipient-only edit; the
//     delivered bit may not.
//
// prev may be nil.
func (e *Engine) InheritStateFrom(prev *Engine, id ReloadIdentity) {
	if prev == nil {
		return
	}
	// Lock both engines. Order is fixed (prev then e) for all callers; a reload
	// builds a brand-new e that no other goroutine can reach yet, so this can't
	// deadlock against a concurrent InheritStateFrom in the other direction.
	prev.mu.Lock()
	defer prev.mu.Unlock()
	e.mu.Lock()
	defer e.mu.Unlock()

	// inheritTarget: target still present with an unchanged measurement identity.
	inheritTarget := func(target string) bool {
		if id.ValidTarget != nil && !id.ValidTarget[target] {
			return false
		}
		if id.SameTarget != nil && !id.SameTarget[target] {
			return false
		}
		return true
	}
	// attached: alert `name` is attached to target `target` in the new config.
	attached := func(target, name string) bool {
		return id.Attached == nil || id.Attached[target][name]
	}
	// sameMatcher: alert defined in both engines with identical matcher semantics.
	sameMatcher := func(name string) bool {
		ea, pa := e.alerts[name], prev.alerts[name]
		return ea != nil && pa != nil && ea.Matcher.Key() == pa.Matcher.Key()
	}
	// inheritState: matcher firing state for (target,name) may carry — target identity,
	// current attachment, and matcher semantics all unchanged.
	inheritState := func(target, name string) bool {
		return inheritTarget(target) && attached(target, name) && sameMatcher(name)
	}
	// sameDelivery: the recipient set for (target,name) — the alert's To plus the target's
	// alertee — is unchanged, so a previously-delivered FIRING is still meaningful to inherit.
	sameDelivery := func(target, name string) bool {
		if id.SameAlertee != nil && !id.SameAlertee[target] {
			return false
		}
		return sameToRecipients(prev.alerts[name], e.alerts[name])
	}

	for key, w := range prev.win {
		_, target, ok := strings.Cut(key, "\x00") // key is vantage\x00target
		if !ok || !inheritTarget(target) {
			continue
		}
		e.win[key] = &Window{
			Loss: append([]float64(nil), w.Loss...),
			RTT:  append([]float64(nil), w.RTT...),
		}
	}
	for key, firing := range prev.state {
		target, name, ok := splitStateKey(key) // key is vantage\x00target\x00name
		if !ok || !inheritState(target, name) {
			continue
		}
		e.state[key] = firing
	}
	// Carry the delivered-firing view too, so a reload mid-inhibition neither
	// resurrects an orphan RESOLVED nor drops a real one — but only when the delivery
	// recipients are unchanged, so a routing edit gives its current recipients a coherent
	// FIRING/RESOLVED lifecycle rather than inheriting a stale "already delivered" bit.
	for key, vis := range prev.visible {
		target, name, ok := splitStateKey(key)
		if !ok || !inheritState(target, name) || !sameDelivery(target, name) {
			continue
		}
		e.visible[key] = vis
	}
}

// sameToRecipients reports whether two alerts route to the same notifier set (order-independent).
func sameToRecipients(a, b *Alert) bool {
	return a != nil && b != nil && sameStringSet(a.To, b.To)
}

// sameStringSet reports whether two string slices contain the same elements, ignoring order.
func sameStringSet(x, y []string) bool {
	if len(x) != len(y) {
		return false
	}
	xs := append([]string(nil), x...)
	ys := append([]string(nil), y...)
	sort.Strings(xs)
	sort.Strings(ys)
	for i := range xs {
		if xs[i] != ys[i] {
			return false
		}
	}
	return true
}

// Evaluate pushes a new sample for (target, vantage), runs the attached alerts, updates
// state, and returns any events produced this round. Each vantage keeps an independent
// window and firing state, so alerts fire per measuring location (P2-5).
func (e *Engine) Evaluate(target, vantage string, alertNames []string, lossPct, rttSec float64, when time.Time) []Event {
	e.mu.Lock()
	defer e.mu.Unlock()

	cap := e.windowCap(alertNames)
	wk := winKey(vantage, target)
	w := e.win[wk]
	if w == nil {
		w = &Window{}
		e.win[wk] = w
	}
	w.Loss = appendCap(w.Loss, lossPct, cap)
	w.RTT = appendCap(w.RTT, rttSec, cap)

	// First pass: update every alert's matcher (firing) state. State must update
	// for all alerts regardless of priority, so hysteresis stays correct even for
	// suppressed alerts. We also find the current inhibitor here.
	type pending struct {
		a      *Alert
		firing bool
	}
	var pend []pending
	// Highest priority (lowest number >= 1) currently firing on this target —
	// the inhibitor. 0 means none, so nothing is suppressed.
	topFiring := 0
	for _, name := range alertNames {
		a := e.alerts[name]
		if a == nil {
			continue
		}
		key := stKey(vantage, target, name)
		prev := e.state[key]
		now := a.Matcher.Test(*w, prev)
		e.state[key] = now

		pend = append(pend, pending{a: a, firing: now})
		if now && a.Priority >= 1 && (topFiring == 0 || a.Priority < topFiring) {
			topFiring = a.Priority
		}
	}

	// Second pass: derive the recipient-visible transitions, applying priority
	// inhibition. Emission is keyed to `visible` (the delivered view) rather than
	// the raw matcher edge, so an edge-triggered alert whose rising edge was
	// swallowed while inhibited still surfaces FIRING once its inhibitor clears —
	// there is no second matcher edge to rely on. The `visible` flag also gates
	// RESOLVED: a suppressed alert never emits an orphan RESOLVED, and one that was
	// delivered before being inhibited still gets its close-out.
	var events []Event
	rttms := rttSec * 1000
	for _, p := range pend {
		key := stKey(vantage, target, p.a.Name)
		if p.firing {
			// Suppress while a strictly-higher-priority alert fires on this target.
			if p.a.Priority >= 1 && topFiring != 0 && p.a.Priority > topFiring {
				continue // inhibited this round; leave `visible` unchanged
			}
			// Active and uninhibited. An edge-triggered alert surfaces FIRING on the
			// transition into the delivered view (whenever it is not already visible —
			// covering both a fresh edge and one that fired earlier under inhibition).
			// A level-triggered alert re-emits every active round (repeat notification),
			// matching SmokePing's non-edge behavior.
			if p.a.EdgeTrigger && e.visible[key] {
				continue // already delivered; an edge alert does not repeat
			}
			e.visible[key] = true
		} else {
			if !e.visible[key] {
				continue // nothing was ever delivered — don't emit an orphan RESOLVED
			}
			e.visible[key] = false
		}
		events = append(events, Event{
			Target: target, Vantage: vantage, Alert: p.a.Name, Comment: p.a.Comment, Firing: p.firing,
			LossPct: lossPct, RTTms: rttms, When: when,
		})
	}
	return events
}

// Dispatch routes each event to its alert's To notifiers, plus any extra
// per-target recipients (a target's alertee). Each distinct recipient is
// notified at most once per event.
func (e *Engine) Dispatch(events []Event, extra ...string) {
	for _, ev := range events {
		a := e.alerts[ev.Alert]
		if a == nil {
			continue
		}
		seen := map[string]bool{}
		for _, name := range append(append([]string{}, a.To...), extra...) {
			if seen[name] {
				continue
			}
			seen[name] = true
			if n := e.notifiers[name]; n != nil {
				n.Notify(ev)
			} else {
				e.warnUnknownNotifier(name, ev.Alert)
			}
		}
	}
}

// warnUnknownNotifier logs (once per name) that an alert or alertee references a
// notifier that does not exist, so a config typo does not silently discard every
// notification routed to it.
func (e *Engine) warnUnknownNotifier(name, alertName string) {
	e.warnMu.Lock()
	if e.warnedUnknown == nil {
		e.warnedUnknown = map[string]bool{}
	}
	first := !e.warnedUnknown[name]
	e.warnedUnknown[name] = true
	e.warnMu.Unlock()
	if first {
		slog.Warn("alert: unknown notifier referenced; its notifications are dropped",
			"notifier", name, "alert", alertName)
	}
}

func appendCap(s []float64, v float64, cap int) []float64 {
	s = append(s, v)
	if len(s) > cap {
		s = s[len(s)-cap:]
	}
	return s
}

// windowCap is the deepest trailing-sample window any of the attached alerts needs (the
// max matcher Length), so a target's Window holds exactly enough history and no more.
func (e *Engine) windowCap(alertNames []string) int {
	cap := 1
	for _, name := range alertNames {
		if a := e.alerts[name]; a != nil {
			if l := a.Matcher.Length(); l > cap {
				cap = l
			}
		}
	}
	return cap
}

// SeedWindow pre-fills a target's sample window from historical loss (percent) and rtt
// (seconds, NaN for a lost round) series, oldest->newest, trimmed to the deepest matcher
// window among the target's alerts. On boot this lets a target that is already breaching
// fire on its first new round instead of waiting X fresh samples — the durable-store
// replacement for SmokePing's S startup sentinel. It seeds only the window, never firing
// state, so it emits no events by itself; a reload carries state via InheritStateFrom.
func (e *Engine) SeedWindow(target, vantage string, alertNames []string, loss, rtt []float64) {
	cap := e.windowCap(alertNames)
	e.mu.Lock()
	defer e.mu.Unlock()
	e.win[winKey(vantage, target)] = &Window{Loss: tailCopy(loss, cap), RTT: tailCopy(rtt, cap)}
}

// tailCopy returns a fresh slice holding the last n elements of s (all of s if shorter).
func tailCopy(s []float64, n int) []float64 {
	if len(s) > n {
		s = s[len(s)-n:]
	}
	return append([]float64(nil), s...)
}

// ---- notifiers ----

// LogNotifier writes a line per event.
type LogNotifier struct{ W io.Writer }

func (n LogNotifier) Notify(e Event) {
	rtt := fmt.Sprintf("%.1fms", e.RTTms)
	if math.IsNaN(e.RTTms) {
		rtt = "--"
	}
	target := e.Target
	if e.Vantage != "" {
		target = e.Target + "@" + e.Vantage
	}
	fmt.Fprintf(n.W, "[ALERT %s] %s / %s — %s (loss %.0f%%, rtt %s) @ %s\n",
		e.Status(), target, e.Alert, e.Comment, e.LossPct, rtt, e.When.Format(time.RFC3339))
}

// WebhookNotifier POSTs each event as JSON through a bounded worker pool with
// retry/backoff, so a slow or unavailable webhook can neither spawn unbounded
// goroutines nor silently lose deliveries. Construct it with NewWebhookNotifier; the
// zero value is not usable. Delivery guarantee: best-effort, at-least-once attempted
// with bounded retries and a stable X-Idempotency-Key (the receiver can dedupe retries
// and level-triggered repeats). On a full queue the newest event is dropped and
// counted; on shutdown Close drains the queue up to a deadline, then abandons and
// counts the remainder — nothing is ever dropped silently.
type WebhookNotifier struct {
	URL    string
	Name   string // for metrics labels ("webhook"/"slack"/"discord"); set by the caller, optional
	Client *http.Client
	format func(Event) ([]byte, error) // Event -> request body; webhookBody by default, or slack/discord

	queue       chan delivery
	done        chan struct{}      // closed by Close to interrupt in-flight backoffs
	baseCtx     context.Context    // parent of every attempt's request context
	baseCancel  context.CancelFunc // cancels in-flight requests when the drain deadline hits
	wg          sync.WaitGroup
	maxAttempts int
	baseBackoff time.Duration
	timeout     time.Duration

	mu     sync.Mutex
	closed bool

	queued, delivered, retried, dropped, failed atomic.Int64
}

// WebhookConfig tunes the delivery pool. Zero fields fall back to defaults.
type WebhookConfig struct {
	Workers     int
	QueueSize   int
	MaxAttempts int
	BaseBackoff time.Duration
	Timeout     time.Duration
}

// WebhookStats is a snapshot of the delivery counters.
type WebhookStats struct {
	Queued, Delivered, Retried, Dropped, Failed int64
	QueueDepth                                  int
}

type delivery struct {
	body          []byte
	idem          string
	alert, target string
}

// NewWebhookNotifier builds a delivery pool with production defaults (4 workers, a
// 1024-deep queue, 4 attempts, 500ms base backoff, 8s per-attempt timeout).
func NewWebhookNotifier(url string, client *http.Client) *WebhookNotifier {
	return NewWebhookNotifierConfig(url, client, WebhookConfig{})
}

// NewWebhookNotifierConfig builds a delivery pool and starts its workers. Zero config
// fields take the defaults; this variant lets tests use a tiny backoff.
func NewWebhookNotifierConfig(url string, client *http.Client, cfg WebhookConfig) *WebhookNotifier {
	if client == nil {
		client = &http.Client{}
	}
	if cfg.Workers <= 0 {
		cfg.Workers = 4
	}
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = 1024
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = 4
	}
	if cfg.BaseBackoff <= 0 {
		cfg.BaseBackoff = 500 * time.Millisecond
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 8 * time.Second
	}
	n := &WebhookNotifier{
		URL: url, Client: client, format: webhookBody,
		queue:       make(chan delivery, cfg.QueueSize),
		done:        make(chan struct{}),
		maxAttempts: cfg.MaxAttempts,
		baseBackoff: cfg.BaseBackoff,
		timeout:     cfg.Timeout,
	}
	// Every attempt's request derives from baseCtx, so Close can cancel in-flight
	// requests once the drain deadline is reached (not just interrupt backoffs).
	n.baseCtx, n.baseCancel = context.WithCancel(context.Background())
	for i := 0; i < cfg.Workers; i++ {
		n.wg.Add(1)
		go n.worker()
	}
	return n
}

// webhookPayload is the wire shape for a webhook delivery. RTTms is a pointer so a
// fully-lost round (NaN median) serializes as JSON null instead of failing the
// entire marshal: Go's encoder rejects NaN, which previously left the body nil and
// POSTed an empty request for exactly the outage case alerting matters most.
type webhookPayload struct {
	Target  string    `json:"target"`
	Vantage string    `json:"vantage,omitempty"`
	Alert   string    `json:"alert"`
	Comment string    `json:"comment"`
	Firing  bool      `json:"firing"`
	Status  string    `json:"status"`
	LossPct float64   `json:"loss_pct"`
	RTTms   *float64  `json:"rtt_ms"`
	When    time.Time `json:"when"`
}

func (n *WebhookNotifier) Notify(e Event) {
	body, err := n.format(e)
	if err != nil {
		// Never silently drop the delivery — surface it so an operator sees the gap.
		slog.Error("notifier: marshal event failed", "alert", e.Alert, "target", e.Target, "err", err)
		return
	}
	d := delivery{body: body, idem: idemKey(e), alert: e.Alert, target: e.Target}
	// Non-blocking enqueue under the lock: never block the alert-eval path, and never
	// race Close's channel close. A full queue drops the newest event, counted.
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed {
		return
	}
	select {
	case n.queue <- d:
		n.queued.Add(1)
	default:
		n.dropped.Add(1)
		slog.Warn("webhook: delivery queue full, dropping event", "url", n.URL, "alert", e.Alert, "target", e.Target, "queue_size", cap(n.queue))
	}
}

// idemKey is a stable per-event key, so every retry and every level-triggered repeat
// of the same event carries one X-Idempotency-Key the receiver can dedupe on. Vantage
// is part of the identity so the same alert firing from two vantages isn't deduped to one.
func idemKey(e Event) string {
	return fmt.Sprintf("%s|%s|%s|%s|%d", e.Vantage, e.Target, e.Alert, strings.ToLower(e.Status()), e.When.UnixNano())
}

func (n *WebhookNotifier) worker() {
	defer n.wg.Done()
	for d := range n.queue {
		n.deliver(d)
	}
}

// deliver POSTs one event, retrying with exponential backoff up to maxAttempts. The
// backoff wait is interrupted by Close, so a shutdown drain doesn't stall on a
// down endpoint.
func (n *WebhookNotifier) deliver(d delivery) {
	backoff := n.baseBackoff
	for attempt := 1; ; attempt++ {
		if n.tryOnce(d) {
			n.delivered.Add(1)
			return
		}
		if attempt >= n.maxAttempts {
			n.failed.Add(1)
			slog.Warn("webhook: giving up after retries", "url", n.URL, "alert", d.alert, "target", d.target, "attempts", attempt)
			return
		}
		n.retried.Add(1)
		select {
		case <-n.done: // draining on shutdown: stop retrying this event
			n.failed.Add(1)
			return
		case <-time.After(backoff):
		}
		backoff *= 2
	}
}

func (n *WebhookNotifier) tryOnce(d delivery) bool {
	ctx, cancel := context.WithTimeout(n.baseCtx, n.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.URL, bytes.NewReader(d.body))
	if err != nil {
		slog.Error("webhook: build request failed", "url", n.URL, "alert", d.alert, "err", err)
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Idempotency-Key", d.idem)
	resp, err := n.Client.Do(req)
	if err != nil {
		slog.Warn("webhook: delivery attempt failed", "url", n.URL, "alert", d.alert, "target", d.target, "err", err)
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		slog.Warn("webhook: non-2xx response", "url", n.URL, "alert", d.alert, "target", d.target, "status", resp.StatusCode)
		return false
	}
	return true
}

// Close stops accepting new events, drains the queue (workers finish what's queued),
// and waits for the workers up to ctx's deadline; past the deadline it returns and the
// remaining deliveries are abandoned. Safe to call more than once.
func (n *WebhookNotifier) Close(ctx context.Context) {
	n.mu.Lock()
	if n.closed {
		n.mu.Unlock()
		return
	}
	n.closed = true
	close(n.queue) // stop accepting; workers finish the remaining items then exit their range loop
	n.mu.Unlock()

	// Do NOT interrupt retries yet: a graceful drain should spend its deadline retrying
	// queued events (an edge-triggered FIRING is emitted once, so a single failed attempt
	// would lose it). n.done is closed only when the deadline hits (#9).
	drained := make(chan struct{})
	go func() { n.wg.Wait(); close(drained) }()
	select {
	case <-drained:
		close(n.done)  // everything delivered/given-up within budget
		n.baseCancel() // release the base context
		slog.Info("webhook: delivery drained", n.statsKV()...)
	case <-ctx.Done():
		// Deadline hit: stop the remaining backoff waits and cancel in-flight requests so a
		// hung endpoint can't keep a worker (and its HTTP request) past the shutdown budget.
		close(n.done)
		n.baseCancel()
		<-drained
		slog.Warn("webhook: drain deadline exceeded; in-flight deliveries cancelled", n.statsKV()...)
	}
}

func (n *WebhookNotifier) statsKV() []any {
	s := n.Stats()
	return []any{"queued", s.Queued, "delivered", s.Delivered, "retried", s.Retried, "dropped", s.Dropped, "failed", s.Failed}
}

// Stats snapshots the delivery counters.
func (n *WebhookNotifier) Stats() WebhookStats {
	return WebhookStats{
		Queued: n.queued.Load(), Delivered: n.delivered.Load(), Retried: n.retried.Load(),
		Dropped: n.dropped.Load(), Failed: n.failed.Load(), QueueDepth: len(n.queue),
	}
}

// WriteMetrics appends the webhook delivery counters in Prometheus text format —
// wired into /metrics via api.Server.ExtraMetrics so drops/failures are scrapeable,
// not merely logged.
func (n *WebhookNotifier) WriteMetrics(b *strings.Builder) {
	s := n.Stats()
	fmt.Fprintf(b, "# HELP smokeping_webhook_queued_total Webhook events accepted onto the delivery queue.\n# TYPE smokeping_webhook_queued_total counter\nsmokeping_webhook_queued_total %d\n", s.Queued)
	fmt.Fprintf(b, "# HELP smokeping_webhook_delivered_total Webhook events delivered (a 2xx response).\n# TYPE smokeping_webhook_delivered_total counter\nsmokeping_webhook_delivered_total %d\n", s.Delivered)
	fmt.Fprintf(b, "# HELP smokeping_webhook_retried_total Webhook delivery attempts retried after a failure.\n# TYPE smokeping_webhook_retried_total counter\nsmokeping_webhook_retried_total %d\n", s.Retried)
	fmt.Fprintf(b, "# HELP smokeping_webhook_dropped_total Webhook events dropped because the delivery queue was full.\n# TYPE smokeping_webhook_dropped_total counter\nsmokeping_webhook_dropped_total %d\n", s.Dropped)
	fmt.Fprintf(b, "# HELP smokeping_webhook_failed_total Webhook events abandoned after exhausting retries.\n# TYPE smokeping_webhook_failed_total counter\nsmokeping_webhook_failed_total %d\n", s.Failed)
	fmt.Fprintf(b, "# HELP smokeping_webhook_queue_depth Current webhook delivery queue depth.\n# TYPE smokeping_webhook_queue_depth gauge\nsmokeping_webhook_queue_depth %d\n", s.QueueDepth)
}

// ---- matcher factory (for `type: matcher`) ----

var matcherRe = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9]*)\((.*)\)$`)

// ParseMatcher compiles "CheckLoss(l=50,x=3)" / "CheckLatency(l=200,x=3)".
// For CheckLatency, l is milliseconds.
func ParseMatcher(spec string) (Matcher, error) {
	m := matcherRe.FindStringSubmatch(strings.TrimSpace(spec))
	if m == nil {
		return nil, fmt.Errorf("matcher %q: expected Name(args)", spec)
	}
	name := m[1]
	args, err := parseArgs(m[2])
	if err != nil {
		return nil, fmt.Errorf("matcher %q: %w", spec, err)
	}
	switch name {
	case "CheckLoss":
		l, x, err := requireLX(args)
		if err != nil {
			return nil, fmt.Errorf("matcher %q: %w", spec, err)
		}
		if l > 100 {
			return nil, fmt.Errorf("matcher %q: loss threshold l must be in (0, 100], got %g", spec, l)
		}
		return CheckLoss{L: l, X: x}, nil
	case "CheckLatency":
		l, x, err := requireLX(args)
		if err != nil {
			return nil, fmt.Errorf("matcher %q: %w", spec, err)
		}
		return CheckLatency{L: l / 1000, X: x}, nil
	default:
		return nil, fmt.Errorf("matcher %q: unknown matcher %s", spec, name)
	}
}

// maxAlertX caps the consecutive-sample window x. Real alerts use a handful of
// samples; a bound keeps a typo (or a hostile config) from sizing a huge per-target
// window buffer.
const maxAlertX = 10000

// maxPatternLength bounds a pattern's AGGREGATE window depth (Pattern.Length) — each *N*
// skip is individually capped at maxAlertX, but their sum could still size a huge per-target
// buffer, so cap the whole thing at two full skips' worth.
const maxPatternLength = 2 * maxAlertX

// requireLX validates the l (threshold) and x (consecutive samples) args common to
// the hysteresis matchers. A missing, zero, non-finite, unknown, or fractional arg
// would otherwise produce a silently broken alert — x=0 never raises (dead), l=0
// makes the threshold always-true (fires forever), x=1.9 truncates to 1, an unknown
// key is a silently-ignored typo — so all are a config error at parse time. Values
// are already finite here (parseArgs rejects NaN/Inf).
func requireLX(args map[string]float64) (l float64, x int, err error) {
	for k := range args {
		if k != "l" && k != "x" {
			return 0, 0, fmt.Errorf("unknown arg %q (only l and x are valid)", k)
		}
	}
	lv, ok := args["l"]
	if !ok {
		return 0, 0, fmt.Errorf("missing required arg l")
	}
	if lv <= 0 {
		return 0, 0, fmt.Errorf("arg l must be > 0, got %g", lv)
	}
	xv, ok := args["x"]
	if !ok {
		return 0, 0, fmt.Errorf("missing required arg x")
	}
	if xv != math.Trunc(xv) {
		return 0, 0, fmt.Errorf("arg x must be a whole number, got %g", xv)
	}
	if xv < 1 || xv > maxAlertX {
		return 0, 0, fmt.Errorf("arg x must be in [1, %d], got %g", maxAlertX, xv)
	}
	return lv, int(xv), nil
}

func parseArgs(s string) (map[string]float64, error) {
	out := map[string]float64{}
	for _, kv := range strings.Split(s, ",") {
		kv = strings.TrimSpace(kv)
		if kv == "" {
			continue
		}
		parts := strings.SplitN(kv, "=", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("bad arg %q", kv)
		}
		key := strings.TrimSpace(parts[0])
		if _, dup := out[key]; dup {
			return nil, fmt.Errorf("duplicate arg %q", key)
		}
		v, err := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
		if err != nil {
			return nil, fmt.Errorf("bad arg %q: %w", kv, err)
		}
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return nil, fmt.Errorf("arg %q must be a finite number, got %q", key, strings.TrimSpace(parts[1]))
		}
		out[key] = v
	}
	return out, nil
}
