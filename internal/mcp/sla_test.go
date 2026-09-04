package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

func TestSLAForwardsWindowAndMaxLoss(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("window") != "48h" || q.Get("maxloss") != "5" {
			t.Errorf("params not forwarded: %v", q)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"window": "48h", "targets": []map[string]any{
			{"id": "a", "name": "x", "probe": "icmp", "availability": 99.5, "measured": 100, "up_rounds": 99},
		}})
	}))
	out, err := fetchSLA(context.Background(), c, "48h", "5", "")
	if err != nil || len(out) != 1 || out[0].Availability != 99.5 {
		t.Fatalf("fetchSLA: out=%+v err=%v", out, err)
	}
}

func TestVantagesReadsFederationReady(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"federation_ready": true,
			"vantages":         []map[string]any{{"name": "local", "created": "2026-01-01T00:00:00Z", "last_seen": nil}},
		})
	}))
	vs, ready, err := fetchVantages(context.Background(), c)
	if err != nil || !ready || len(vs) != 1 || vs[0].Name != "local" {
		t.Fatalf("fetchVantages: vs=%+v ready=%v err=%v", vs, ready, err)
	}
}
