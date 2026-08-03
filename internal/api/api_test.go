package api

import (
	"net/http/httptest"
	"strings"
	"testing"

	"smokeping-modern/internal/probe"
	"smokeping-modern/internal/sample"
	"smokeping-modern/internal/scheduler"
	"smokeping-modern/internal/store"
)

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
}
