package allprobes

import (
	"testing"

	"github.com/seitzbg/heliograph/internal/probe"
)

// Importing this package must register every built-in probe kind. Both binaries depend on this
// single list, so this test is the guard that a probe added to one is available to the other —
// the drift that made NTP an "unknown probe" at remote vantages (CODE_REVIEW M2).
func TestAllProbesRegistered(t *testing.T) {
	want := []string{"DNS", "FPing", "HTTP", "IRTT", "NTP", "Ping", "SSH", "TCPConnect"}
	got := map[string]bool{}
	for _, k := range probe.Registered() {
		got[k] = true
	}
	for _, k := range want {
		if !got[k] {
			t.Errorf("probe %q is not registered by allprobes", k)
		}
	}
	if len(probe.Registered()) != len(want) {
		t.Errorf("registered kinds = %v, want exactly %v", probe.Registered(), want)
	}
}
