package scheduler

import (
	"context"
	"testing"
	"time"

	"github.com/seitzbg/heliograph/internal/probe"
)

// kindProbe returns a fixed metric kind so we can assert the scheduler copies it onto the Outcome.
type kindProbe struct{ kind string }

func (k *kindProbe) Name() string { return "Fake" }
func (k *kindProbe) Measure(context.Context, probe.Target, int) (probe.Result, error) {
	return probe.Result{Samples: []float64{0.01}, Kind: k.kind}, nil
}

func TestOutcomeMetricFromResultKind(t *testing.T) {
	jobs := []Job{{Probe: &kindProbe{kind: probe.MetricOffset}, Target: probe.Target{Name: "o"}, Pings: 1, Timeout: time.Second}}
	out := RunRound(context.Background(), jobs, 1)
	if out[0].Metric != probe.MetricOffset {
		t.Errorf("Outcome.Metric = %q, want %q (from Result.Kind)", out[0].Metric, probe.MetricOffset)
	}
}

func TestOutcomeMetricDefaultsToRTT(t *testing.T) {
	jobs := []Job{{Probe: &kindProbe{kind: ""}, Target: probe.Target{Name: "r"}, Pings: 1, Timeout: time.Second}}
	out := RunRound(context.Background(), jobs, 1)
	if out[0].Metric != probe.MetricRTT {
		t.Errorf("Outcome.Metric = %q, want %q (empty Kind defaults to rtt)", out[0].Metric, probe.MetricRTT)
	}
}
