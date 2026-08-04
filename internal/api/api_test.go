package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"smokeping-modern/internal/probe"
	"smokeping-modern/internal/sample"
	"smokeping-modern/internal/scheduler"
	"smokeping-modern/internal/store"
)

// rollupInternalErr stands in for a raw database error carrying internal detail
// (table names, driver internals) that must not reach an API caller.
const rollupInternalErr = `pq: relation "samples_hourly" does not exist`

// errRollupStore is a Rollupper whose Rollup always fails with an internal error.
type errRollupStore struct{ *store.MemStore }

func (errRollupStore) Rollup(context.Context, string) ([]store.RollupPoint, error) {
	return nil, errors.New(rollupInternalErr)
}

func TestRollupHidesInternalError(t *testing.T) {
	srv := New(errRollupStore{store.NewMem(10)}, "")
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, httptest.NewRequest("GET", "/api/rollup?target=x", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, rollupInternalErr) {
		t.Errorf("response leaked the internal DB error to the client:\n%s", body)
	}
	if !strings.Contains(body, "rollup unavailable") {
		t.Errorf("expected a generic error message, got:\n%s", body)
	}
}

func TestMetricsEndpoint(t *testing.T) {
	st := store.NewMem(10)
	st.Add([]scheduler.Outcome{
		{Target: probe.Target{Name: "a b/c", Host: "h"}, ProbeName: "FPing",
			Computed: sample.Compute(4, []float64{0.01, 0.02, 0.03, 0.04})}, // no loss, median 0.03
		{Target: probe.Target{Name: "down", Host: "h"}, ProbeName: "TCPConnect",
			Computed: sample.Compute(4, nil)}, // total loss
	})
	srv := New(st, "")

	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()

	want := []string{
		`smokeping_probe_median_seconds{target="a b/c",probe="FPing"} 0.03`,
		`smokeping_probe_loss_ratio{target="a b/c",probe="FPing"} 0`,
		`smokeping_probe_up{target="a b/c",probe="FPing"} 1`,
		`smokeping_probe_median_seconds{target="down",probe="TCPConnect"} NaN`,
		`smokeping_probe_loss_ratio{target="down",probe="TCPConnect"} 1`,
		`smokeping_probe_up{target="down",probe="TCPConnect"} 0`,
		"# TYPE smokeping_probe_median_seconds gauge",
	}
	for _, s := range want {
		if !strings.Contains(body, s) {
			t.Errorf("metrics output missing line:\n  %s\n--- got ---\n%s", s, body)
		}
	}
	// Without a RoundStats, no round metrics are emitted.
	if strings.Contains(body, "smokeping_rounds_total") {
		t.Errorf("round metrics emitted without a RoundStats:\n%s", body)
	}
}

func TestChartsRanking(t *testing.T) {
	st := store.NewMem(10)
	st.Add([]scheduler.Outcome{
		// low latency, no loss
		{Target: probe.Target{Name: "fast", Host: "h"}, ProbeName: "FPing",
			Computed: sample.Compute(4, []float64{0.01, 0.011, 0.012, 0.013})},
		// high latency, no loss, high jitter
		{Target: probe.Target{Name: "slow", Host: "h"}, ProbeName: "FPing",
			Computed: sample.Compute(4, []float64{0.10, 0.20, 0.30, 0.40})},
		// fully lost -> 100% loss, no median/stddev
		{Target: probe.Target{Name: "down", Host: "h"}, ProbeName: "TCPConnect",
			Computed: sample.Compute(4, nil)},
	})
	srv := New(st, "")

	get := func(q string) map[string]any {
		rec := httptest.NewRecorder()
		srv.Routes().ServeHTTP(rec, httptest.NewRequest("GET", "/api/charts"+q, nil))
		if rec.Code != 200 {
			t.Fatalf("%s: status %d", q, rec.Code)
		}
		var m map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return m
	}
	names := func(m map[string]any) []string {
		var out []string
		for _, c := range m["charts"].([]any) {
			out = append(out, c.(map[string]any)["name"].(string))
		}
		return out
	}

	// by=loss (default): "down" (100%) first.
	if got := names(get("")); len(got) == 0 || got[0] != "down" {
		t.Errorf("by=loss: got %v, want down first", got)
	}
	// by=median: "down" excluded (no median); "slow" ranks above "fast".
	got := names(get("?by=median"))
	if contains(got, "down") {
		t.Errorf("by=median must exclude the fully-lost target, got %v", got)
	}
	if len(got) < 2 || got[0] != "slow" {
		t.Errorf("by=median: got %v, want slow first", got)
	}
	// by=stddev: "slow" (spread 0.1..0.4) beats "fast" (tight); "down" excluded.
	got = names(get("?by=stddev"))
	if len(got) == 0 || got[0] != "slow" || contains(got, "down") {
		t.Errorf("by=stddev: got %v, want slow first, no down", got)
	}
	// n caps the result.
	if got := names(get("?by=loss&n=1")); len(got) != 1 {
		t.Errorf("n=1: got %d entries, want 1", len(got))
	}
	// invalid by -> 400.
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, httptest.NewRequest("GET", "/api/charts?by=bogus", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("by=bogus: status %d, want 400", rec.Code)
	}
}

func contains(s []string, want string) bool {
	for _, v := range s {
		if v == want {
			return true
		}
	}
	return false
}

func TestProbeSchemaEndpoint(t *testing.T) {
	srv := New(store.NewMem(1), "")
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, httptest.NewRequest("GET", "/api/probes/schema", nil))
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	// The real probes are registered by cmd/smoked's blank imports, not here, so
	// this test asserts the envelope + JSON Schema markers are emitted; the shape
	// of an individual probe's schema is covered by probe.TestJSONSchemaShape.
	for _, want := range []string{`"probes"`, "application/json"} {
		if want == "application/json" {
			if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, want) {
				t.Errorf("Content-Type = %q, want %s", ct, want)
			}
			continue
		}
		if !strings.Contains(body, want) {
			t.Errorf("schema response missing %s:\n%s", want, body)
		}
	}
}

func TestMetricsPerProbeDurationAndRoundStats(t *testing.T) {
	st := store.NewMem(10)
	st.Add([]scheduler.Outcome{
		{Target: probe.Target{Name: "a", Host: "h"}, ProbeName: "FPing",
			Computed: sample.Compute(2, []float64{0.01, 0.02}), Duration: 250 * time.Millisecond},
	})
	srv := New(st, "")
	srv.Rounds = &RoundStats{}
	srv.Rounds.Observe(1500*time.Millisecond, 12, 3, time.Unix(1_700_000_000, 0))

	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	body := rec.Body.String()

	want := []string{
		`smokeping_probe_duration_seconds{target="a",probe="FPing"} 0.25`,
		"smokeping_rounds_total 1",
		"smokeping_round_duration_seconds 1.5",
		"smokeping_round_targets 12",
		"smokeping_round_errors 3",
		"smokeping_last_round_timestamp_seconds 1700000000",
	}
	for _, s := range want {
		if !strings.Contains(body, s) {
			t.Errorf("metrics output missing line:\n  %s\n--- got ---\n%s", s, body)
		}
	}
}
