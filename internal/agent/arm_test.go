package agent

import (
	"testing"
	"time"

	"smokeping-modern/internal/agentwire"

	_ "smokeping-modern/internal/probe/tcpconnect" // register a real, binary-free probe
)

func TestBuildJobsValidAndSkips(t *testing.T) {
	targets := []agentwire.AssignmentTarget{
		{Name: "ok", Probe: "TCPConnect", Host: "1.1.1.1", Params: map[string]string{"port": "443"}, StepMs: 1000, Pings: 3},
		{Name: "unknown-probe", Probe: "NoSuchProbe", Host: "x", StepMs: 1000, Pings: 1},
		{Name: "bad-param", Probe: "TCPConnect", Host: "x", Params: map[string]string{"port": "notaport"}, StepMs: 1000, Pings: 1},
		{Name: "unknown-param", Probe: "TCPConnect", Host: "x", Params: map[string]string{"nope": "1"}, StepMs: 1000, Pings: 1},
	}
	jobs, skipped := BuildJobs(targets, 4*time.Second)
	if len(jobs) != 1 || jobs[0].Target.Name != "ok" {
		t.Fatalf("jobs=%+v", jobs)
	}
	if jobs[0].Pings != 3 || jobs[0].Step != time.Second || jobs[0].Timeout != 4*time.Second {
		t.Fatalf("job fields: %+v", jobs[0])
	}
	if len(skipped) != 3 {
		t.Fatalf("skipped=%v (want 3)", skipped)
	}
}
