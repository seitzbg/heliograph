package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

func TestStatusToolReturnsTargets(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("vantage") != "munro-fios" {
			t.Errorf("vantage not forwarded: %q", r.URL.Query().Get("vantage"))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"targets": []map[string]any{
			{"id": "a", "name": "Websites/github", "probe": "http", "median_ms": 42.5, "loss_pct": 0.0, "when": "2026-09-04T00:00:00Z"},
		}})
	}))
	out, err := fetchStatus(context.Background(), c, "munro-fios")
	if err != nil {
		t.Fatalf("fetchStatus: %v", err)
	}
	if len(out) != 1 || out[0].Name != "Websites/github" || out[0].MedianMs == nil || *out[0].MedianMs != 42.5 {
		t.Fatalf("unexpected targets: %+v", out)
	}
}
