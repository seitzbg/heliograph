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
	if got := reportFromOutcome(o, nil); got.Fingerprint != "sha256:cafef00d" {
		t.Fatalf("RoundReport.Fingerprint = %q, want sha256:cafef00d", got.Fingerprint)
	}

	o.Fingerprint = ""
	if got := reportFromOutcome(o, nil); got.Fingerprint != "" {
		t.Fatalf("empty Outcome.Fingerprint should stay empty, got %q", got.Fingerprint)
	}
}

// reportFromOutcome must echo the target's stable id (not its display Name) as
// RoundReport.Target when the assignment carried one, so the hub attributes the round to the
// same storage identity regardless of the target's current tree path. When the id is empty
// (an old hub that never sent one), it falls back to Name — Target.Key()'s contract.
func TestReportFromOutcomeTargetUsesIDElseName(t *testing.T) {
	when := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	o := scheduler.Outcome{
		Target:    probe.Target{ID: "wid", Name: "grp/leaf", Host: "h"},
		ProbeName: "FPing",
		Computed:  sample.Compute(1, []float64{0.01}),
		When:      when,
	}
	if got := reportFromOutcome(o, nil); got.Target != "wid" {
		t.Fatalf("RoundReport.Target = %q, want the id %q", got.Target, "wid")
	}

	o.Target.ID = "" // old-hub compat: no id was assigned
	if got := reportFromOutcome(o, nil); got.Target != "grp/leaf" {
		t.Fatalf("RoundReport.Target = %q, want fallback to Name %q", got.Target, "grp/leaf")
	}
}

// An NTP outcome carries the probe's companion clock stat (offset ms + stratum) onto the wire
// RoundReport via the injected lookup, so a remote vantage's clock reading reaches the hub. A
// non-NTP outcome, or an unsynchronized clock (ok=false), leaves the fields absent (CODE_REVIEW M2).
func TestReportFromOutcomeCarriesNTPStat(t *testing.T) {
	when := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	base := scheduler.Outcome{
		Target:   probe.Target{Name: "clock", Host: "h"},
		Computed: sample.Compute(2, []float64{0.001, 0.002}),
		When:     when,
	}
	stat := func(target string) (float64, uint8, string, bool) {
		if target == "clock" {
			return -0.0009, 2, "rtt", true // -0.9 ms, stratum 2
		}
		return 0, 0, "", false
	}

	ntp := base
	ntp.ProbeName = "NTP"
	got := reportFromOutcome(ntp, stat)
	if got.NTPOffsetMs == nil || *got.NTPOffsetMs != -0.9 || got.Stratum == nil || *got.Stratum != 2 {
		t.Fatalf("NTP outcome must carry offset/stratum, got off=%v st=%v", got.NTPOffsetMs, got.Stratum)
	}

	// Non-NTP outcome: never attach the stat, even with a lookup that would answer.
	http := base
	http.ProbeName = "HTTP"
	if got := reportFromOutcome(http, stat); got.NTPOffsetMs != nil || got.Stratum != nil {
		t.Fatalf("non-NTP outcome must omit offset/stratum, got off=%v st=%v", got.NTPOffsetMs, got.Stratum)
	}

	// Unsynchronized clock: lookup returns ok=false for an unknown target, so nothing is attached.
	unknown := base
	unknown.ProbeName = "NTP"
	unknown.Target.Name = "silent"
	if got := reportFromOutcome(unknown, stat); got.NTPOffsetMs != nil || got.Stratum != nil {
		t.Fatalf("no-stat NTP round must omit offset/stratum, got off=%v st=%v", got.NTPOffsetMs, got.Stratum)
	}
}
