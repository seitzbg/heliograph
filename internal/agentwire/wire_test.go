package agentwire

import (
	"encoding/json"
	"testing"
)

func TestAssignmentJSONTags(t *testing.T) {
	a := Assignment{Vantage: "nyc", ConfigVersion: "sha256:x", Targets: []AssignmentTarget{
		{Name: "cf", Probe: "FPing", Host: "1.1.1.1", StepMs: 60000, Pings: 20},
	}}
	b, _ := json.Marshal(a)
	want := `{"vantage":"nyc","config_version":"sha256:x","targets":[{"name":"cf","probe":"FPing","host":"1.1.1.1","step_ms":60000,"pings":20}]}`
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
