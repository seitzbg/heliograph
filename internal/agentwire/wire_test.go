package agentwire

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestAssignmentJSONTags(t *testing.T) {
	a := Assignment{Vantage: "nyc", ConfigVersion: "sha256:x", Targets: []AssignmentTarget{
		{Name: "cf", Probe: "FPing", Host: "1.1.1.1", StepMs: 60000, Pings: 20},
	}}
	b, _ := json.Marshal(a)
	// id is a core identity field like name/probe/host: no omitempty, so it always rides
	// the wire even when a caller (like this test fixture) left it unset.
	want := `{"vantage":"nyc","config_version":"sha256:x","targets":[{"id":"","name":"cf","probe":"FPing","host":"1.1.1.1","step_ms":60000,"pings":20}]}`
	if string(b) != want {
		t.Fatalf("assignment json:\n got %s\nwant %s", b, want)
	}
}

func TestRoundReportOmitEmpty(t *testing.T) {
	r := RoundReport{Target: "cf", TS: "2026-08-07T00:00:00Z", Pings: 3, RTTs: []float64{0.01}}
	b, _ := json.Marshal(r)
	want := `{"target":"cf","ts":"2026-08-07T00:00:00Z","pings":3,"rtts":[0.01]}`
	if string(b) != want {
		t.Fatalf("round json:\n got %s\nwant %s", b, want)
	}
}

// The optional NTP clock stat round-trips when present and stays absent when nil, so an old hub
// ignores it and a new one reads it back exactly (CODE_REVIEW M2).
func TestRoundReportNTPStatRoundTrip(t *testing.T) {
	off, st := -0.0009*1000, 2
	r := RoundReport{Target: "clock", TS: "2026-08-07T00:00:00Z", Pings: 2, RTTs: []float64{0.001, 0.002},
		NTPOffsetMs: &off, Stratum: &st}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"ntp_offset_ms":-0.9`) || !strings.Contains(string(b), `"stratum":2`) {
		t.Fatalf("ntp stat not on the wire: %s", b)
	}
	var back RoundReport
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.NTPOffsetMs == nil || *back.NTPOffsetMs != -0.9 || back.Stratum == nil || *back.Stratum != 2 {
		t.Fatalf("round-trip lost the stat: off=%v st=%v", back.NTPOffsetMs, back.Stratum)
	}
}
