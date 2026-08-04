package alert

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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
	state     map[string]bool
}

func NewEngine(alerts map[string]*Alert, notifiers map[string]Notifier) *Engine {
	return &Engine{
		alerts:    alerts,
		notifiers: notifiers,
		win:       map[string]*Window{},
		state:     map[string]bool{},
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

	var events []Event
	for _, name := range alertNames {
		a := e.alerts[name]
		if a == nil {
			continue
		}
		key := target + "\x00" + name
		prev := e.state[key]
		now := a.Matcher.Test(*w, prev)
		e.state[key] = now

		emit := false
		firing := now
		if a.EdgeTrigger {
			emit = now != prev // only on transition
		} else {
			// non-edge: emit while firing, plus one resolved on the clearing edge
			emit = now || prev
		}
		if emit {
			rttms := rttSec * 1000
			events = append(events, Event{
				Target: target, Alert: name, Comment: a.Comment, Firing: firing,
				LossPct: lossPct, RTTms: rttms, When: when,
			})
		}
	}
	return events
}

// Dispatch routes each event to the notifiers named in its alert's To list.
func (e *Engine) Dispatch(events []Event) {
	for _, ev := range events {
		a := e.alerts[ev.Alert]
		if a == nil {
			continue
		}
		for _, name := range a.To {
			if n := e.notifiers[name]; n != nil {
				n.Notify(ev)
			}
		}
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

func (n WebhookNotifier) Notify(e Event) {
	body, _ := json.Marshal(struct {
		Event
		Status string `json:"status"`
	}{Event: e, Status: strings.ToLower(e.Status())})
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.URL, bytes.NewReader(body))
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", "application/json")
		c := n.Client
		if c == nil {
			c = http.DefaultClient
		}
		resp, err := c.Do(req)
		if err != nil {
			return
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
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
