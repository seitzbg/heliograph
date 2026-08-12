package irttprobe

import (
	"math"
	"testing"

	"github.com/seitzbg/heliograph/internal/probe"
)

// interval_ms is a send interval and must be positive: 0 (silently ignored by the
// factory before) is now a loud config error (#9).
func TestIntervalMsMustBePositive(t *testing.T) {
	s, ok := probe.SchemaOf("IRTT")
	if !ok {
		t.Fatal("IRTT not registered")
	}
	if err := s["interval_ms"].ValidateValue("interval_ms", "0"); err == nil {
		t.Error("interval_ms=0 should be rejected")
	}
	if err := s["interval_ms"].ValidateValue("interval_ms", "20"); err != nil {
		t.Errorf("interval_ms=20 should be accepted: %v", err)
	}
}

// Fixture mirrors real irtt JSON: rtt in nanoseconds, and `lost` is the STRING
// "false" for received packets ("true_up"/etc. for losses). The last entry uses
// a boolean to confirm we also tolerate that older form.
const fixture = `{
  "round_trips": [
    {"seqno": 0, "lost": "false",   "delay": {"rtt": 71309}},
    {"seqno": 1, "lost": "true_up", "delay": {"rtt": 0}},
    {"seqno": 2, "lost": "false",   "delay": {"rtt": 120500}},
    {"seqno": 3, "lost": false,     "delay": {"rtt": 90000}}
  ]
}`

func TestParseIRTT(t *testing.T) {
	samples, err := parseIRTT([]byte(fixture))
	if err != nil {
		t.Fatalf("parseIRTT: %v", err)
	}
	if len(samples) != 3 { // one packet lost -> excluded
		t.Fatalf("got %d samples, want 3: %v", len(samples), samples)
	}
	// 71309 ns == 0.000071309 s
	if math.Abs(samples[0]-0.000071309) > 1e-12 {
		t.Errorf("sample0 = %v, want ~7.13e-5", samples[0])
	}
}

func TestParseIRTTEmpty(t *testing.T) {
	if _, err := parseIRTT(nil); err == nil {
		t.Errorf("expected error on empty output")
	}
	if _, err := parseIRTT([]byte("not json")); err == nil {
		t.Errorf("expected error on bad json")
	}
}
