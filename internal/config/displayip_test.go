package config

import (
	"testing"

	"github.com/seitzbg/heliograph/internal/model"
)

func TestDisplayIP(t *testing.T) {
	// lookup stub: only "example.com" resolves; used to prove pinned/literal skip it.
	lookup := func(host string) []string {
		if host == "example.com" {
			return []string{"93.184.216.34"}
		}
		return nil
	}

	tests := []struct {
		name   string
		mon    model.Monitor
		lookup func(string) []string
		want   string
	}{
		{"pinned wins over host+resolve", model.Monitor{Host: "example.com", IP: "1.1.1.1"}, lookup, "1.1.1.1"},
		{"literal IPv4 host, no pin", model.Monitor{Host: "8.8.8.8"}, lookup, "8.8.8.8"},
		{"literal IPv6 host, no pin", model.Monitor{Host: "2606:4700:4700::1111"}, lookup, "2606:4700:4700::1111"},
		{"hostname resolves", model.Monitor{Host: "example.com"}, lookup, "93.184.216.34"},
		{"hostname unresolved -> empty", model.Monitor{Host: "nope.invalid"}, lookup, ""},
		{"nil lookup, hostname -> empty", model.Monitor{Host: "example.com"}, nil, ""},
	}
	for _, tc := range tests {
		if got := DisplayIP(tc.mon, tc.lookup); got != tc.want {
			t.Errorf("%s: DisplayIP = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestDisplayIPMultiDedupeSortCap: several resolved addresses are deduped, sorted for
// stable output, and capped so a title can't grow unbounded.
func TestDisplayIPMultiDedupeSortCap(t *testing.T) {
	many := func(string) []string {
		return []string{"10.0.0.3", "10.0.0.1", "10.0.0.3", "10.0.0.2", "10.0.0.5", "10.0.0.4"}
	}
	got := DisplayIP(model.Monitor{Host: "h.example"}, many)
	// deduped (6->5 unique), sorted, capped at maxDisplayIPs, comma-joined.
	want := "10.0.0.1, 10.0.0.2, 10.0.0.3"
	if got != want {
		t.Errorf("DisplayIP multi = %q, want %q (deduped, sorted, capped at %d)", got, want, maxDisplayIPs)
	}
}
