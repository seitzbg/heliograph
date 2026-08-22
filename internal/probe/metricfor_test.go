package probe

import "testing"

func TestMetricFor(t *testing.T) {
	cases := []struct {
		name       string
		kind       string
		tp, pp     map[string]string
		want       string
	}{
		{"non-NTP is rtt", "FPing", nil, nil, MetricRTT},
		{"NTP default rtt", "NTP", nil, nil, MetricRTT},
		{"per-target offset", "NTP", map[string]string{"measure": "offset"}, nil, MetricOffset},
		{"probe-level offset default", "NTP", nil, map[string]string{"measure": "offset"}, MetricOffset},
		{"target overrides probe-level", "NTP", map[string]string{"measure": "rtt"}, map[string]string{"measure": "offset"}, MetricRTT},
	}
	for _, c := range cases {
		if got := MetricFor(c.kind, c.tp, c.pp); got != c.want {
			t.Errorf("%s: MetricFor(%q,%v,%v) = %q, want %q", c.name, c.kind, c.tp, c.pp, got, c.want)
		}
	}
}
