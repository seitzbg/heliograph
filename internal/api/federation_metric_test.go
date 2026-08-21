package api

import (
	"fmt"
	"testing"
	"time"

	"github.com/seitzbg/heliograph/internal/model"
	"github.com/seitzbg/heliograph/internal/probe"
)

// M2: an offset-mode NTP target measured at a remote vantage reports signed offset samples. The
// hub must accept the negative values (the RTT-only >=0 gate would reject them) and stamp the
// round with the metric derived from the AUTHENTICATED assignment, not the agent's self-report.
func TestIngestOffsetNTPAcceptsSignedSamples(t *testing.T) {
	ing := &fakeIngester{}
	srv := &Server{
		store:       ing,
		VantageAuth: fakeAuth{name: "nyc", ok: true},
		Assignment: func(string) ([]model.Monitor, map[string]map[string]string, string) {
			return []model.Monitor{{Name: "clk", ProbeKind: "NTP", Host: "ntp1", Pings: 3, Step: time.Minute,
				Vantages: []string{"nyc"}, Params: map[string]string{"measure": "offset"}}}, nil, "sha256:v1"
		},
	}
	w := postResults(t, srv,
		fmt.Sprintf(`{"results":[{"target":"clk","ts":%q,"pings":3,"rtts":[-0.001,-0.0009,-0.0011]}]}`, recentTS()))
	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body)
	}
	if len(ing.got) != 1 {
		t.Fatalf("offset round with negative samples must be accepted, stored %d", len(ing.got))
	}
	o := ing.got[0]
	if o.Metric != probe.MetricOffset {
		t.Errorf("ingested Outcome.Metric = %q, want offset (from the assignment)", o.Metric)
	}
	if o.Computed.Median > -0.0005 {
		t.Errorf("median = %v, want ~-0.001 (negative offset preserved through Compute)", o.Computed.Median)
	}
}

// The signed allowance is scoped to offset assignments: a negative sample on an rtt target is still
// bogus (an agent bug or hostile client) and must be dropped.
func TestIngestRTTStillRejectsNegative(t *testing.T) {
	ing := &fakeIngester{}
	srv := ingestServer(ing) // "cf" is FPing (rtt)
	w := postResults(t, srv,
		fmt.Sprintf(`{"results":[{"target":"cf","ts":%q,"pings":3,"rtts":[-0.01,0.02,0.03]}]}`, recentTS()))
	if w.Code != 200 {
		t.Fatalf("status=%d", w.Code)
	}
	if len(ing.got) != 0 {
		t.Errorf("a negative RTT sample on an rtt target must drop the round, stored %d", len(ing.got))
	}
}
