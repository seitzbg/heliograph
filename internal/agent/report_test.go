package agent

import (
	"testing"
	"time"

	"github.com/seitzbg/heliograph/internal/probe"
	"github.com/seitzbg/heliograph/internal/sample"
	"github.com/seitzbg/heliograph/internal/scheduler"
)

// reportFromOutcome must copy the opaque Fingerprint tag from the Outcome onto the
// wire RoundReport, so the buffered round carries the identity of the assignment that
// produced it across a store-and-forward replay. An empty tag stays empty.
func TestReportFromOutcomeCarriesFingerprint(t *testing.T) {
	when := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	o := scheduler.Outcome{
		Target:      probe.Target{Name: "t", Host: "h"},
		ProbeName:   "FPing",
		Computed:    sample.Compute(2, []float64{0.01, 0.02}),
		When:        when,
		Duration:    50 * time.Millisecond,
		Fingerprint: "sha256:cafef00d",
	}
	if got := reportFromOutcome(o); got.Fingerprint != "sha256:cafef00d" {
		t.Fatalf("RoundReport.Fingerprint = %q, want sha256:cafef00d", got.Fingerprint)
	}

	o.Fingerprint = ""
	if got := reportFromOutcome(o); got.Fingerprint != "" {
		t.Fatalf("empty Outcome.Fingerprint should stay empty, got %q", got.Fingerprint)
	}
}
