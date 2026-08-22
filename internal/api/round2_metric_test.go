package api

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/seitzbg/heliograph/internal/model"
	"github.com/seitzbg/heliograph/internal/probe"
	"github.com/seitzbg/heliograph/internal/sample"
	"github.com/seitzbg/heliograph/internal/scheduler"
	"github.com/seitzbg/heliograph/internal/store"
)

// M5/M6 (round 2): the metric is chosen from the target's EFFECTIVE CONFIG (via EffectiveMetric),
// not the newest stored row. Right after a target switches to offset, /api/targets reports offset
// even though only rtt rounds are stored, and /api/series echoes offset and drops the now-mismatched
// rtt history (rather than rendering it on the signed axis).
func TestEffectiveMetricDrivesConfigMetric(t *testing.T) {
	st := store.NewMem(20)
	base := time.Unix(1_700_000_000, 0)
	st.Add([]scheduler.Outcome{
		{Target: probe.Target{Name: "clk", Host: "h"}, ProbeName: "NTP", Metric: probe.MetricRTT,
			Computed: sample.Compute(2, []float64{0.02, 0.02}), When: base},
		{Target: probe.Target{Name: "clk", Host: "h"}, ProbeName: "NTP", Metric: probe.MetricRTT,
			Computed: sample.Compute(2, []float64{0.02, 0.02}), When: base.Add(time.Minute)},
	})
	srv := New(st, "")
	srv.Configured = func() []model.Monitor { return []model.Monitor{{Name: "clk", ProbeKind: "NTP"}} }
	srv.EffectiveMetric = func(target string) string {
		if target == "clk" {
			return probe.MetricOffset // config now says offset (e.g. probe-level default)
		}
		return probe.MetricRTT
	}

	// /api/targets: metric follows config, not the rtt-tagged stored rounds.
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, httptest.NewRequest("GET", "/api/targets", nil))
	var tj struct {
		Targets []struct {
			Name, Metric string
		} `json:"targets"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &tj); err != nil {
		t.Fatalf("targets json: %v", err)
	}
	if len(tj.Targets) != 1 || tj.Targets[0].Metric != probe.MetricOffset {
		t.Errorf("/api/targets clk metric = %+v, want offset (from config)", tj.Targets)
	}

	// /api/series: echoes the config metric and drops the mismatched rtt rounds.
	rec = httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, httptest.NewRequest("GET", "/api/series?target=clk", nil))
	var sj struct {
		Metric string            `json:"metric"`
		Rounds []json.RawMessage `json:"rounds"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &sj); err != nil {
		t.Fatalf("series json: %v", err)
	}
	if sj.Metric != probe.MetricOffset {
		t.Errorf("series metric = %q, want offset (config, not newest row)", sj.Metric)
	}
	if len(sj.Rounds) != 0 {
		t.Errorf("series returned %d rounds, want 0 (rtt history must not render on the offset axis)", len(sj.Rounds))
	}
}
