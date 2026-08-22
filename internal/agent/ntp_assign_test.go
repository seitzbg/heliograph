package agent

import (
	"testing"
	"time"

	"github.com/seitzbg/heliograph/internal/agentwire"

	_ "github.com/seitzbg/heliograph/internal/probe/ntpprobe" // the registration the agent binary now carries
)

// M2: an NTP target assigned to a vantage must build a real job — before the agent registered
// ntpprobe it was dropped into `skipped` as `unknown probe "NTP"` and never measured.
func TestBuildJobsBuildsNTPOffsetTarget(t *testing.T) {
	targets := []agentwire.AssignmentTarget{
		{Name: "clk", Probe: "NTP", Host: "ntp1", Params: map[string]string{"measure": "offset"}, StepMs: 60000, Pings: 5},
	}
	jobs, skipped := BuildJobs(targets, 4*time.Second)
	if len(skipped) != 0 {
		t.Fatalf("NTP target was skipped: %v", skipped)
	}
	if len(jobs) != 1 || jobs[0].Probe.Name() != "NTP" {
		t.Fatalf("want 1 NTP job, got %+v", jobs)
	}
}
