package pgstore

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/seitzbg/heliograph/internal/probe"
	"github.com/seitzbg/heliograph/internal/sample"
	"github.com/seitzbg/heliograph/internal/scheduler"
)

// A round's metric kind (rtt vs offset) round-trips through the store, and an untagged round
// defaults to rtt.
func TestMetricRoundTrips(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	when := time.Unix(1_700_300_000, 0).UTC()

	s.Add([]scheduler.Outcome{
		{Target: probe.Target{Name: "clk", Host: "h"}, ProbeName: "NTP", Metric: probe.MetricOffset,
			Computed: sample.Compute(3, []float64{-0.001, -0.0009, -0.0011}), When: when},
		{Target: probe.Target{Name: "lat", Host: "h"}, ProbeName: "FPing", Metric: probe.MetricRTT,
			Computed: sample.Compute(3, []float64{0.02, 0.021, 0.019}), When: when},
		{Target: probe.Target{Name: "untagged", Host: "h"}, ProbeName: "FPing", // Metric: "" -> rtt
			Computed: sample.Compute(3, []float64{0.03, 0.031, 0.029}), When: when},
	})

	for name, wantMetric := range map[string]string{"clk": probe.MetricOffset, "lat": probe.MetricRTT, "untagged": probe.MetricRTT} {
		o, ok := s.Latest(name)
		if !ok {
			t.Fatalf("%s: no latest row", name)
		}
		if o.Metric != wantMetric {
			t.Errorf("%s: Latest().Metric = %q, want %q", name, o.Metric, wantMetric)
		}
		h, err := s.HistoryVantage(ctx, name, "local")
		if err != nil || len(h) != 1 {
			t.Fatalf("%s: HistoryVantage len=%d err=%v", name, len(h), err)
		}
		if h[0].Metric != wantMetric {
			t.Errorf("%s: History[0].Metric = %q, want %q", name, h[0].Metric, wantMetric)
		}
	}
}

// The rollup aggregates must GROUP BY metric so a target that carries both rtt and offset rounds
// in one bucket is not averaged into a single meaningless number — it yields one rollup point per
// metric, each with the right sign.
func TestRollupGroupsByMetric(t *testing.T) {
	dsn := os.Getenv("SMOKE_TEST_DSN")
	if dsn == "" {
		t.Skip("set SMOKE_TEST_DSN to run the TimescaleDB integration test")
	}
	ctx := context.Background()
	s, err := New(ctx, dsn, 100, func(e error) { t.Errorf("store error: %v", e) })
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()
	if _, err := s.pool.Exec(ctx, "DELETE FROM samples WHERE target = 'mix'"); err != nil {
		t.Fatal(err)
	}
	if err := s.EnableDownsampling(ctx); err != nil {
		t.Fatalf("EnableDownsampling: %v", err)
	}

	// Both metrics in the SAME hour bucket, older than the retention edge but materializable.
	base := time.Now().Add(-40 * 24 * time.Hour).Truncate(time.Hour).Add(5 * time.Minute)
	var outs []scheduler.Outcome
	for i := 0; i < 3; i++ {
		when := base.Add(time.Duration(i) * time.Minute)
		outs = append(outs,
			scheduler.Outcome{Target: probe.Target{Name: "mix", Host: "h"}, ProbeName: "FPing", Metric: probe.MetricRTT,
				Computed: sample.Compute(3, []float64{0.02, 0.02, 0.02}), When: when},
			scheduler.Outcome{Target: probe.Target{Name: "mix", Host: "h"}, ProbeName: "NTP", Metric: probe.MetricOffset,
				Computed: sample.Compute(3, []float64{-0.001, -0.001, -0.001}), When: when.Add(30 * time.Second)},
		)
	}
	s.Add(outs)
	if err := s.RefreshAggregates(ctx, base.Add(-time.Hour), time.Now()); err != nil {
		t.Fatalf("RefreshAggregates: %v", err)
	}

	pts, err := s.Rollup(ctx, "mix", "local", "1h", base.Add(-2*time.Hour), time.Now())
	if err != nil {
		t.Fatalf("Rollup: %v", err)
	}
	byMetric := map[string]store_RollupMedian{}
	for _, p := range pts {
		byMetric[p.Metric] = store_RollupMedian{p.MedianAvg}
	}
	if len(byMetric) != 2 {
		t.Fatalf("rollup produced %d distinct metrics, want 2 (rtt+offset not merged); points=%+v", len(byMetric), pts)
	}
	if m := byMetric[probe.MetricRTT].avg; m < 0.015 || m > 0.025 {
		t.Errorf("rtt rollup median_avg = %v, want ~0.02", m)
	}
	if m := byMetric[probe.MetricOffset].avg; m > -0.0005 {
		t.Errorf("offset rollup median_avg = %v, want ~-0.001 (negative, not merged with rtt)", m)
	}
}

type store_RollupMedian struct{ avg float64 }
