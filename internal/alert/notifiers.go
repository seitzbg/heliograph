package alert

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"
)

// This file holds the wire-body formatters for the delivery pool in engine.go. Slack and Discord
// reuse WebhookNotifier's bounded-queue + retry/backoff + drain machinery verbatim; only the request
// body differs, so each is just a formatter plus a thin constructor.

// webhookBody is the default generic JSON payload (the documented webhook wire shape). RTTms is a
// pointer so a fully-lost round (NaN median) serializes as JSON null instead of failing the marshal.
func webhookBody(e Event) ([]byte, error) {
	var rtt *float64
	if !math.IsNaN(e.RTTms) && !math.IsInf(e.RTTms, 0) {
		v := e.RTTms
		rtt = &v
	}
	return json.Marshal(webhookPayload{
		Target: e.Target, Vantage: e.Vantage, Alert: e.Alert, Comment: e.Comment, Firing: e.Firing,
		Status: strings.ToLower(e.Status()), LossPct: e.LossPct, RTTms: rtt, When: e.When,
	})
}

// alertMessage renders an Event as one human-readable line (plus the comment on a second line, if
// any) — the shared text body for the chat notifiers. A lost round (NaN/Inf median) renders "n/a"
// rather than a bogus number.
func alertMessage(e Event) string {
	vant := ""
	if e.Vantage != "" && e.Vantage != "local" {
		vant = " @" + e.Vantage
	}
	rtt := "n/a"
	if !math.IsNaN(e.RTTms) && !math.IsInf(e.RTTms, 0) {
		rtt = fmt.Sprintf("%.1f ms", e.RTTms)
	}
	msg := fmt.Sprintf("[%s] %s on %s%s — loss %.1f%%, median %s (%s)",
		e.Status(), e.Alert, e.Target, vant, e.LossPct, rtt,
		e.When.UTC().Format("2006-01-02 15:04:05 MST"))
	if e.Comment != "" {
		msg += "\n" + e.Comment
	}
	return msg
}

// slackBody is a Slack incoming-webhook payload: {"text": "..."}.
func slackBody(e Event) ([]byte, error) {
	return json.Marshal(map[string]string{"text": alertMessage(e)})
}

// discordBody is a Discord webhook payload: {"content": "..."}.
func discordBody(e Event) ([]byte, error) {
	return json.Marshal(map[string]string{"content": alertMessage(e)})
}

// NewSlackNotifier posts alerts to a Slack incoming webhook, reusing the WebhookNotifier delivery
// pool. cfg is the same knobs as NewWebhookNotifierConfig (zero values take the defaults).
func NewSlackNotifier(url string, client *http.Client, cfg WebhookConfig) *WebhookNotifier {
	n := NewWebhookNotifierConfig(url, client, cfg)
	n.format = slackBody
	return n
}

// NewDiscordNotifier posts alerts to a Discord webhook, reusing the WebhookNotifier delivery pool.
func NewDiscordNotifier(url string, client *http.Client, cfg WebhookConfig) *WebhookNotifier {
	n := NewWebhookNotifierConfig(url, client, cfg)
	n.format = discordBody
	return n
}

// WriteNotifierMetrics writes the delivery counters for a set of webhook-family notifiers in
// Prometheus text format: HELP/TYPE once per metric family, then one sample per notifier keyed by a
// `notifier` label. Labeling avoids the name collision that unlabeled per-notifier WriteMetrics would
// cause once webhook + slack + discord are all configured, while a query on the bare metric name
// still matches every notifier.
func WriteNotifierMetrics(b *strings.Builder, ns []*WebhookNotifier) {
	if len(ns) == 0 {
		return
	}
	families := []struct {
		name, help, typ string
		val             func(WebhookStats) int64
	}{
		{"heliograph_webhook_queued_total", "Notifier events accepted onto the delivery queue.", "counter", func(s WebhookStats) int64 { return s.Queued }},
		{"heliograph_webhook_delivered_total", "Notifier events delivered (a 2xx response).", "counter", func(s WebhookStats) int64 { return s.Delivered }},
		{"heliograph_webhook_retried_total", "Notifier delivery attempts retried after a failure.", "counter", func(s WebhookStats) int64 { return s.Retried }},
		{"heliograph_webhook_dropped_total", "Notifier events dropped because the delivery queue was full.", "counter", func(s WebhookStats) int64 { return s.Dropped }},
		{"heliograph_webhook_failed_total", "Notifier events abandoned after exhausting retries.", "counter", func(s WebhookStats) int64 { return s.Failed }},
		{"heliograph_webhook_queue_depth", "Current notifier delivery queue depth.", "gauge", func(s WebhookStats) int64 { return int64(s.QueueDepth) }},
	}
	stats := make([]WebhookStats, len(ns))
	for i, n := range ns {
		stats[i] = n.Stats()
	}
	for _, f := range families {
		fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s %s\n", f.name, f.help, f.name, f.typ)
		for i, n := range ns {
			name := n.Name
			if name == "" {
				name = "webhook"
			}
			fmt.Fprintf(b, "%s{notifier=%q} %d\n", f.name, name, f.val(stats[i]))
		}
	}
}
