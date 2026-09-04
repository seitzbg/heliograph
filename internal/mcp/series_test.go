package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

func TestSeriesCompactDropsRTTsAndCaps(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("target") != "id-1" {
			t.Errorf("target not forwarded")
		}
		rounds := make([]map[string]any, 0, 3)
		for i := 0; i < 3; i++ {
			rounds = append(rounds, map[string]any{"t": "2026-09-04T00:0" + string(rune('0'+i)) + ":00Z", "median_ms": 1.0, "loss": 0, "pings": 20, "rtts_ms": []any{1.0, 1.1}})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"target": "id-1", "metric": "rtt", "rounds": rounds})
	}))
	metric, rs, err := fetchSeries(context.Background(), c, "id-1", "", "3h", 0, 0, false, 2)
	if err != nil {
		t.Fatalf("fetchSeries: %v", err)
	}
	if metric != "rtt" || len(rs) != 2 { // capped to newest 2
		t.Fatalf("metric=%q rounds=%d", metric, len(rs))
	}
	if rs[0].RTTsMs != nil {
		t.Fatalf("compact mode must drop rtts_ms, got %v", rs[0].RTTsMs)
	}
}

func TestSeriesDetailKeepsRTTs(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"target": "id-1", "metric": "rtt", "rounds": []map[string]any{
			{"t": "2026-09-04T00:00:00Z", "median_ms": 1.0, "loss": 0, "pings": 20, "rtts_ms": []any{1.0, 1.1}},
		}})
	}))
	_, rs, err := fetchSeries(context.Background(), c, "id-1", "", "", 0, 0, true, 500)
	if err != nil || len(rs) != 1 || len(rs[0].RTTsMs) != 2 {
		t.Fatalf("detail: rs=%+v err=%v", rs, err)
	}
}
