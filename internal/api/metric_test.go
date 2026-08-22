package api

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/seitzbg/heliograph/internal/probe"
	"github.com/seitzbg/heliograph/internal/sample"
	"github.com/seitzbg/heliograph/internal/scheduler"
	"github.com/seitzbg/heliograph/internal/store"
)

// M4: an offset-metric target's value must NOT be exported under the RTT median gauge (whose HELP
// says "round-trip time"); it goes to an offset-specific gauge. RTT targets keep the median gauge,
// and loss/up stay for both.
func TestMetricsSeparatesOffsetFromRTT(t *testing.T) {
	st := store.NewMem(10)
	st.Add([]scheduler.Outcome{
		{Target: probe.Target{Name: "lat", Host: "h"}, ProbeName: "FPing", Metric: probe.MetricRTT,
			Computed: sample.Compute(2, []float64{0.02, 0.02}), When: time.Unix(1_700_000_000, 0)},
		{Target: probe.Target{Name: "clk", Host: "h"}, ProbeName: "NTP", Metric: probe.MetricOffset,
			Computed: sample.Compute(2, []float64{-0.001, -0.001}), When: time.Unix(1_700_000_000, 0)},
	})
	srv := New(st, "")
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	body := rec.Body.String()

	if !strings.Contains(body, `heliograph_probe_median_seconds{target="lat",probe="FPing"} 0.02`) {
		t.Errorf("rtt target missing from median gauge:\n%s", body)
	}
	if strings.Contains(body, `heliograph_probe_median_seconds{target="clk"`) {
		t.Errorf("offset target must not appear under the RTT median gauge:\n%s", body)
	}
	if !strings.Contains(body, `heliograph_ntp_offset_seconds{target="clk",probe="NTP"} -0.001`) {
		t.Errorf("offset target missing from the offset gauge:\n%s", body)
	}
	if !strings.Contains(body, `heliograph_probe_up{target="clk",probe="NTP"}`) {
		t.Errorf("offset target should still export up/loss (metric-agnostic):\n%s", body)
	}
}

// M6: /api/targets exposes a per-target metric derived from the stored round (config-resolved),
// so the signed-axis decision survives even when no live NTP offset stat exists (NTPStat unset).
func TestTargetsExposesMetric(t *testing.T) {
	st := store.NewMem(10)
	st.Add([]scheduler.Outcome{
		{Target: probe.Target{Name: "clk", Host: "h"}, ProbeName: "NTP", Metric: probe.MetricOffset,
			Computed: sample.Compute(2, []float64{-0.001, -0.001}), When: time.Unix(1_700_000_000, 0)},
		{Target: probe.Target{Name: "lat", Host: "h"}, ProbeName: "FPing", Metric: probe.MetricRTT,
			Computed: sample.Compute(2, []float64{0.02, 0.02}), When: time.Unix(1_700_000_000, 0)},
	})
	srv := New(st, "") // no NTPStat wired: the metric must still come through
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, httptest.NewRequest("GET", "/api/targets", nil))

	var tj struct {
		Targets []struct {
			Name   string `json:"name"`
			Metric string `json:"metric"`
		} `json:"targets"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &tj); err != nil {
		t.Fatalf("targets json: %v", err)
	}
	got := map[string]string{}
	for _, x := range tj.Targets {
		got[x.Name] = x.Metric
	}
	if got["clk"] != probe.MetricOffset {
		t.Errorf("clk metric = %q, want %q", got["clk"], probe.MetricOffset)
	}
	if got["lat"] != probe.MetricRTT {
		t.Errorf("lat metric = %q, want %q", got["lat"], probe.MetricRTT)
	}
}

// M5: a target that changed measure returns mixed-kind history; /api/series must return only the
// current-metric rounds (never rtt and offset on one axis) and echo that metric.
func TestSeriesFiltersToCurrentMetric(t *testing.T) {
	st := store.NewMem(20)
	base := time.Unix(1_700_000_000, 0)
	st.Add([]scheduler.Outcome{
		{Target: probe.Target{Name: "clk", Host: "h"}, ProbeName: "NTP", Metric: probe.MetricRTT,
			Computed: sample.Compute(2, []float64{0.02, 0.02}), When: base},
		{Target: probe.Target{Name: "clk", Host: "h"}, ProbeName: "NTP", Metric: probe.MetricRTT,
			Computed: sample.Compute(2, []float64{0.02, 0.02}), When: base.Add(time.Minute)},
		{Target: probe.Target{Name: "clk", Host: "h"}, ProbeName: "NTP", Metric: probe.MetricOffset,
			Computed: sample.Compute(2, []float64{-0.001, -0.001}), When: base.Add(2 * time.Minute)},
	})
	srv := New(st, "")
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, httptest.NewRequest("GET", "/api/series?target=clk", nil))

	var sj struct {
		Metric string            `json:"metric"`
		Rounds []json.RawMessage `json:"rounds"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &sj); err != nil {
		t.Fatalf("series json: %v", err)
	}
	if sj.Metric != probe.MetricOffset {
		t.Errorf("series metric = %q, want offset", sj.Metric)
	}
	if len(sj.Rounds) != 1 {
		t.Errorf("series returned %d rounds, want 1 (only the current offset round, not the 2 old rtt rounds)", len(sj.Rounds))
	}
}
