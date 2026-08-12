package alert

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

var testWhen = time.Date(2026, 8, 12, 10, 0, 0, 0, time.UTC)

func TestAlertMessageFiring(t *testing.T) {
	e := Event{Target: "Cloudflare/443 (TCP)", Alert: "loss", Firing: true, LossPct: 50, RTTms: 12.34, When: testWhen, Comment: "high loss on the uplink"}
	msg := alertMessage(e)
	for _, want := range []string{"FIRING", "loss", "Cloudflare/443 (TCP)", "50.0%", "12.3 ms", "high loss on the uplink"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q missing %q", msg, want)
		}
	}
}

func TestAlertMessageResolvedNoComment(t *testing.T) {
	e := Event{Target: "T", Alert: "latency", Firing: false, LossPct: 0, RTTms: 5, When: testWhen}
	msg := alertMessage(e)
	if !strings.Contains(msg, "RESOLVED") {
		t.Errorf("resolved event should say RESOLVED: %q", msg)
	}
	if strings.Contains(msg, "\n") {
		t.Errorf("no comment => single line, got %q", msg)
	}
}

func TestAlertMessageVantage(t *testing.T) {
	local := alertMessage(Event{Target: "T", Alert: "a", Vantage: "local", When: testWhen})
	if strings.Contains(local, "@") {
		t.Errorf("local vantage should not be tagged: %q", local)
	}
	remote := alertMessage(Event{Target: "T", Alert: "a", Vantage: "nyc", When: testWhen})
	if !strings.Contains(remote, "@nyc") {
		t.Errorf("remote vantage should be tagged: %q", remote)
	}
}

func TestAlertMessageLostRound(t *testing.T) {
	e := Event{Target: "x", Alert: "loss", Firing: true, LossPct: 100, RTTms: math.NaN(), When: testWhen}
	msg := alertMessage(e)
	if !strings.Contains(msg, "n/a") {
		t.Errorf("a lost round (NaN rtt) should render n/a, got %q", msg)
	}
}

func TestSlackBody(t *testing.T) {
	body, err := slackBody(Event{Target: "T", Alert: "loss", Firing: false, When: testWhen})
	if err != nil {
		t.Fatalf("slackBody: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("slack body not valid JSON: %v (%s)", err, body)
	}
	text, ok := m["text"].(string)
	if !ok {
		t.Fatalf("slack body must carry a string \"text\" field: %s", body)
	}
	if !strings.Contains(text, "RESOLVED") {
		t.Errorf("slack text should reflect status: %q", text)
	}
}

func TestDiscordBody(t *testing.T) {
	body, err := discordBody(Event{Target: "T", Alert: "loss", Firing: true, When: testWhen})
	if err != nil {
		t.Fatalf("discordBody: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("discord body not valid JSON: %v (%s)", err, body)
	}
	if _, ok := m["content"].(string); !ok {
		t.Fatalf("discord body must carry a string \"content\" field: %s", body)
	}
}

// A NaN rtt must not break the JSON marshal for either service (Go's encoder rejects NaN).
func TestSlackDiscordBodyHandleNaN(t *testing.T) {
	e := Event{Target: "T", Alert: "loss", Firing: true, LossPct: 100, RTTms: math.NaN(), When: testWhen}
	if _, err := slackBody(e); err != nil {
		t.Errorf("slackBody on lost round: %v", err)
	}
	if _, err := discordBody(e); err != nil {
		t.Errorf("discordBody on lost round: %v", err)
	}
}

// captureOne stands up a one-shot server that records the first request body it receives.
func captureOne(t *testing.T) (*httptest.Server, chan []byte) {
	t.Helper()
	got := make(chan []byte, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		select {
		case got <- b:
		default:
		}
		w.WriteHeader(http.StatusOK)
	}))
	return srv, got
}

func TestSlackNotifierDelivers(t *testing.T) {
	srv, got := captureOne(t)
	defer srv.Close()
	n := NewSlackNotifier(srv.URL, nil, WebhookConfig{Workers: 1, QueueSize: 4, MaxAttempts: 2, BaseBackoff: time.Millisecond, Timeout: time.Second})
	defer n.Close(context.Background())
	n.Notify(Event{Target: "web/443", Alert: "loss", Firing: true, LossPct: 50, RTTms: 12.3, When: testWhen})
	select {
	case b := <-got:
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatalf("delivered body not JSON: %v (%s)", err, b)
		}
		text, _ := m["text"].(string)
		if !strings.Contains(text, "FIRING") || !strings.Contains(text, "web/443") {
			t.Errorf("slack delivery text = %q", text)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("slack notifier did not deliver within 2s")
	}
}

// WriteNotifierMetrics must emit each metric family's HELP/TYPE exactly once and one `notifier`-
// labeled sample per notifier — so webhook + slack + discord don't collide into duplicate metric
// lines. (No delivery happens, so this needs no listener.)
func TestWriteNotifierMetricsLabeled(t *testing.T) {
	cfg := WebhookConfig{Workers: 1, QueueSize: 2, MaxAttempts: 1, BaseBackoff: time.Millisecond, Timeout: time.Second}
	s := NewSlackNotifier("http://example.invalid/s", nil, cfg)
	s.Name = "slack"
	defer s.Close(context.Background())
	d := NewDiscordNotifier("http://example.invalid/d", nil, cfg)
	d.Name = "discord"
	defer d.Close(context.Background())

	var b strings.Builder
	WriteNotifierMetrics(&b, []*WebhookNotifier{s, d})
	out := b.String()
	if got := strings.Count(out, "# HELP smokeping_webhook_queued_total"); got != 1 {
		t.Errorf("HELP for a family must appear exactly once, got %d\n%s", got, out)
	}
	for _, want := range []string{
		`smokeping_webhook_queued_total{notifier="slack"}`,
		`smokeping_webhook_queued_total{notifier="discord"}`,
		`smokeping_webhook_queue_depth{notifier="slack"}`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metrics missing %q\n%s", want, out)
		}
	}
}

func TestDiscordNotifierDelivers(t *testing.T) {
	srv, got := captureOne(t)
	defer srv.Close()
	n := NewDiscordNotifier(srv.URL, nil, WebhookConfig{Workers: 1, QueueSize: 4, MaxAttempts: 2, BaseBackoff: time.Millisecond, Timeout: time.Second})
	defer n.Close(context.Background())
	n.Notify(Event{Target: "web/443", Alert: "loss", Firing: true, LossPct: 50, RTTms: 12.3, When: testWhen})
	select {
	case b := <-got:
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatalf("delivered body not JSON: %v (%s)", err, b)
		}
		if _, ok := m["content"].(string); !ok {
			t.Errorf("discord delivery missing content: %s", b)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("discord notifier did not deliver within 2s")
	}
}
