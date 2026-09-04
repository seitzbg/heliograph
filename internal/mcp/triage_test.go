package mcp

import "testing"

func f(v float64) *float64 { return &v }

func TestClassify(t *testing.T) {
	cases := []struct {
		name string
		tg   Target
		want string
	}{
		{"nodata", Target{NoData: true}, "no_data"},
		{"error-down", Target{Error: "timeout"}, "down"},
		{"loss-down", Target{RecentLossPct: f(100)}, "down"},
		{"degraded", Target{RecentLossPct: f(10)}, "degraded"},
		{"healthy", Target{RecentLossPct: f(0)}, "healthy"},
		{"fallback-losspct", Target{LossPct: 50}, "degraded"},
	}
	for _, tc := range cases {
		if got := classify(tc.tg); got != tc.want {
			t.Errorf("%s: classify=%q want %q", tc.name, got, tc.want)
		}
	}
}

func TestTriageSplitsGlobalVsVantageSpecific(t *testing.T) {
	byV := map[string][]Target{
		"a": {{ID: "g", Name: "G", RecentLossPct: f(100)}, {ID: "v", Name: "V", RecentLossPct: f(100)}},
		"b": {{ID: "g", Name: "G", RecentLossPct: f(100)}, {ID: "v", Name: "V", RecentLossPct: f(0)}},
	}
	probs := analyzeTriage(byV)
	got := map[string]string{}
	for _, p := range probs {
		got[p.Target] = p.Scope
	}
	if got["G"] != "global" || got["V"] != "vantage-specific" {
		t.Fatalf("scopes wrong: %v", got)
	}
}
