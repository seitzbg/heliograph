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

// WebhookNotifier POSTs the event as JSON (fire-and-forget).
type WebhookNotifier struct {
	URL    string
	Client *http.Client
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

func (n WebhookNotifier) Notify(e Event) {
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
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.URL, bytes.NewReader(body))
		if err != nil {
			slog.Error("webhook: build request failed", "url", n.URL, "alert", e.Alert, "err", err)
			return
		}
		req.Header.Set("Content-Type", "application/json")
		c := n.Client
		if c == nil {
			c = http.DefaultClient
		}
		resp, err := c.Do(req)
		if err != nil {
			slog.Warn("webhook: delivery failed", "url", n.URL, "alert", e.Alert, "target", e.Target, "err", err)
			return
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			slog.Warn("webhook: non-2xx response", "url", n.URL, "alert", e.Alert, "target", e.Target, "status", resp.StatusCode)
		}
	}()
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

// requireLX validates the l (threshold) and x (consecutive samples) args common
// to the hysteresis matchers. A missing or zero arg would otherwise produce a
// silently broken alert — x=0 never raises (dead), l=0 makes the threshold
// always-true (fires forever) — so both must be a config error at parse time.
func requireLX(args map[string]float64) (l float64, x int, err error) {
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
	if xv < 1 {
		return 0, 0, fmt.Errorf("arg x must be >= 1, got %g", xv)
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
		v, err := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
		if err != nil {
			return nil, fmt.Errorf("bad arg %q: %w", kv, err)
		}
		out[strings.TrimSpace(parts[0])] = v
	}
	return out, nil
}
