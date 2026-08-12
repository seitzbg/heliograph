package agent

import (
	"context"
	"testing"
	"time"

	"github.com/seitzbg/heliograph/internal/agentwire"
	"github.com/seitzbg/heliograph/internal/probe"

	_ "github.com/seitzbg/heliograph/internal/probe/tcpconnect" // register a real, binary-free probe
)

// captureProbe records the config it was constructed with, so a test can assert
// BuildJobs threads AssignmentTarget.ProbeConfig into probe.New.
type captureProbe struct{ cfg map[string]string }

func (captureProbe) Name() string { return "CaptureCfg" }
func (captureProbe) Measure(context.Context, probe.Target, int) (probe.Result, error) {
	return probe.Result{}, nil
}

var lastCaptureCfg map[string]string

func init() {
	probe.Register("CaptureCfg", "records its construction config", nil,
		func(cfg map[string]string) (probe.Probe, error) {
			lastCaptureCfg = cfg
			return captureProbe{cfg: cfg}, nil
		})
}

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

// BuildJobs must construct each probe with the hub's effective probe-level config
// (AssignmentTarget.ProbeConfig), not bare defaults — CODE_REVIEW #2 / P1-2. Proven
// by a probe that records the config it received.
func TestBuildJobsAppliesProbeConfig(t *testing.T) {
	lastCaptureCfg = nil
	targets := []agentwire.AssignmentTarget{
		{Name: "cap", Probe: "CaptureCfg", Host: "h",
			ProbeConfig: map[string]string{"protocol": "tcp", "binary": "/opt/fping"},
			StepMs:      1000, Pings: 1},
	}
	jobs, skipped := BuildJobs(targets, 4*time.Second)
	if len(jobs) != 1 || len(skipped) != 0 {
		t.Fatalf("jobs=%d skipped=%v", len(jobs), skipped)
	}
	if lastCaptureCfg["protocol"] != "tcp" || lastCaptureCfg["binary"] != "/opt/fping" {
		t.Fatalf("probe not built with hub probe config: got %+v", lastCaptureCfg)
	}
}

// BuildJobs must carry the hub's opaque Fingerprint tag from each AssignmentTarget
// onto its Job, so it can later ride the Outcome into the buffered RoundReport.
func TestBuildJobsCarriesFingerprint(t *testing.T) {
	targets := []agentwire.AssignmentTarget{
		{Name: "ok", Probe: "TCPConnect", Host: "1.1.1.1", Params: map[string]string{"port": "443"}, StepMs: 1000, Pings: 3, Fingerprint: "sha256:deadbeef"},
	}
	jobs, skipped := BuildJobs(targets, 4*time.Second)
	if len(jobs) != 1 || len(skipped) != 0 {
		t.Fatalf("jobs=%d skipped=%v", len(jobs), skipped)
	}
	if jobs[0].Fingerprint != "sha256:deadbeef" {
		t.Fatalf("Job.Fingerprint = %q, want sha256:deadbeef", jobs[0].Fingerprint)
	}
}
