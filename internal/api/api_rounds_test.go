package api

import (
	"strings"
	"testing"
	"time"

	"smokeping-modern/internal/probe"
	"smokeping-modern/internal/sample"
	"smokeping-modern/internal/scheduler"
)

// roundsDTO must serialize sub-second-distinct rounds to distinct timestamps: the grid uses
// the serialized `t` as its incremental cursor, and whole-second precision let two rounds in
// one wall-clock second collide so mergeSeries dropped one (review #3).
func TestRoundsDTOMillisecondPrecision(t *testing.T) {
	base := time.Unix(1_700_000_000, 0).UTC()
	hist := []scheduler.Outcome{
		{Target: probe.Target{Name: "x"}, ProbeName: "FPing", Computed: sample.Compute(1, []float64{0.01}), When: base},
		{Target: probe.Target{Name: "x"}, ProbeName: "FPing", Computed: sample.Compute(1, []float64{0.01}), When: base.Add(500 * time.Millisecond)},
	}
	dto := roundsDTO(hist)
	if len(dto) != 2 || dto[0].T == dto[1].T {
		t.Fatalf("two rounds 500ms apart must serialize to distinct T; got %q and %q", dto[0].T, dto[1].T)
	}
	if !strings.Contains(dto[0].T, ".") {
		t.Errorf("T=%q lacks sub-second precision", dto[0].T)
	}
}
