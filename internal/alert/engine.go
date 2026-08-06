package alert

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"regexp"
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

// Engine tracks per-target sample windows and per-(target,alert) firing state.
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

// InheritStateFrom seeds this engine with the firing state and sample windows of
// a previous engine, so a config reload (which builds a fresh engine) does not
// reset alert hysteresis. Without this a reload would (a) re-emit FIRING for an
// alert that is already firing, spamming notifiers, and (b) drop the windowed
// loss/RTT history, so a currently-firing alert reads as not-firing until X new
// samples re-accumulate — silently resolving mid-outage, then re-raising later.
//
// Only state for targets in validTargets and alerts still defined in this engine
// is carried over, so removed targets/alerts don't leak across reloads. Pass a
// nil validTargets to carry everything (used in tests). prev may be nil.
func (e *Engine) InheritStateFrom(prev *Engine, validTargets map[string]bool) {
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

	for target, w := range prev.win {
		if validTargets != nil && !validTargets[target] {
			continue
		}
		e.win[target] = &Window{
			Loss: append([]float64(nil), w.Loss...),
			RTT:  append([]float64(nil), w.RTT...),
		}
	}
	for key, firing := range prev.state {
		target, name, ok := strings.Cut(key, "\x00")
		if !ok {
			continue
		}
		if _, defined := e.alerts[name]; !defined {
			continue
		}
		if validTargets != nil && !validTargets[target] {
			continue
		}
		e.state[key] = firing
	}
	// Carry the delivered-firing view too, so a reload mid-inhibition neither
	// resurrects an orphan RESOLVED nor drops a real one.
	for key, vis := range prev.visible {
		target, name, ok := strings.Cut(key, "\x00")
		if !ok {
			continue
		}
		if _, defined := e.alerts[name]; !defined {
			continue
		}
		if validTargets != nil && !validTargets[target] {
			continue
		}
		e.visible[key] = vis
	}
}

// Evaluate pushes a new sample for target, runs the attached alerts, updates
// state, and returns any events produced this round.
func (e *Engine) Evaluate(target string, alertNames []string, lossPct, rttSec float64, when time.Time) []Event {
	e.mu.Lock()
	defer e.mu.Unlock()

	// window capacity = max matcher length among the attached alerts
	cap := 1
	for _, name := range alertNames {
		if a := e.alerts[name]; a != nil {
			if l := a.Matcher.Length(); l > cap {
				cap = l
			}
		}
	}
	w := e.win[target]
	if w == nil {
		w = &Window{}
		e.win[target] = w
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
		key := target + "\x00" + name
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
		key := target + "\x00" + p.a.Name
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
			Target: target, Alert: p.a.Name, Comment: p.a.Comment, Firing: p.firing,
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

// ---- notifiers ----

// LogNotifier writes a line per event.
type LogNotifier struct{ W io.Writer }

func (n LogNotifier) Notify(e Event) {
	rtt := fmt.Sprintf("%.1fms", e.RTTms)
	if math.IsNaN(e.RTTms) {
		rtt = "--"
	}
	fmt.Fprintf(n.W, "[ALERT %s] %s / %s — %s (loss %.0f%%, rtt %s) @ %s\n",
		e.Status(), e.Target, e.Alert, e.Comment, e.LossPct, rtt, e.When.Format(time.RFC3339))
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
	Client *http.Client

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
		URL: url, Client: client,
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
	Alert   string    `json:"alert"`
	Comment string    `json:"comment"`
	Firing  bool      `json:"firing"`
	Status  string    `json:"status"`
	LossPct float64   `json:"loss_pct"`
	RTTms   *float64  `json:"rtt_ms"`
	When    time.Time `json:"when"`
}

func (n *WebhookNotifier) Notify(e Event) {
	var rtt *float64
	if !math.IsNaN(e.RTTms) && !math.IsInf(e.RTTms, 0) {
		v := e.RTTms
		rtt = &v
	}
	body, err := json.Marshal(webhookPayload{
		Target: e.Target, Alert: e.Alert, Comment: e.Comment, Firing: e.Firing,
		Status: strings.ToLower(e.Status()), LossPct: e.LossPct, RTTms: rtt, When: e.When,
	})
	if err != nil {
		// Never silently drop the delivery — surface it so an operator sees the gap.
		slog.Error("webhook: marshal event failed", "alert", e.Alert, "target", e.Target, "err", err)
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
// of the same event carries one X-Idempotency-Key the receiver can dedupe on.
func idemKey(e Event) string {
	return fmt.Sprintf("%s|%s|%s|%d", e.Target, e.Alert, strings.ToLower(e.Status()), e.When.UnixNano())
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
	close(n.done)  // interrupt in-flight backoffs so the drain is prompt
	close(n.queue) // workers finish the remaining items then exit their range loop
	n.mu.Unlock()

	drained := make(chan struct{})
	go func() { n.wg.Wait(); close(drained) }()
	select {
	case <-drained:
		n.baseCancel() // release the base context
		slog.Info("webhook: delivery drained", n.statsKV()...)
	case <-ctx.Done():
		// Deadline hit: cancel in-flight requests so a hung endpoint can't keep a worker
		// (and its HTTP request) running past the shutdown budget, then let the workers
		// finish exiting.
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
