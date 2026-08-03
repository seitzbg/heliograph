package irttprobe

import (
	"math"
	"testing"
)

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
