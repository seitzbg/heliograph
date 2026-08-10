package agent

import (
	"fmt"
	"time"

	"smokeping-modern/internal/agentwire"
	"smokeping-modern/internal/config"
	"smokeping-modern/internal/probe"
	"smokeping-modern/internal/scheduler"
)

// BuildJobs validates each assignment target against the local probe registry and
// builds a runnable scheduler.Job for the valid ones. Invalid entries are collected
// in skipped ("<name>: <reason>") and never executed — a bad entry can't take the
// whole assignment down (design §5: the agent never evals unknown/invalid config).
func BuildJobs(targets []agentwire.AssignmentTarget, timeout time.Duration) (jobs []scheduler.Job, skipped []string) {
	for _, t := range targets {
		schema, ok := probe.SchemaOf(t.Probe)
		if !ok {
			skipped = append(skipped, fmt.Sprintf("%s: unknown probe %q", t.Name, t.Probe))
			continue
		}
		bad := ""
		for k, v := range t.Params {
			spec, known := schema[k]
			if !known || spec.Scope != probe.TargetVar {
				bad = fmt.Sprintf("unknown/non-target param %q", k)
				break
			}
			if err := spec.ValidateValue(k, v); err != nil {
				bad = err.Error()
				break
			}
		}
		if bad != "" {
			skipped = append(skipped, fmt.Sprintf("%s: %s", t.Name, bad))
			continue
		}
		// Build the probe with the hub's effective probe-level config for this kind
		// (probes.<Kind>), so a remote probe behaves like the hub's local one; per-target
		// params ride on Target. A bad config errors here and skips just this target.
		p, err := probe.New(t.Probe, t.ProbeConfig)
		if err != nil {
			skipped = append(skipped, fmt.Sprintf("%s: probe construct: %v", t.Name, err))
			continue
		}
		step := time.Duration(t.StepMs) * time.Millisecond
		if step < config.MinStep {
			step = config.MinStep
		}
		jobs = append(jobs, scheduler.Job{
			Probe:   p,
			Target:  probe.Target{Name: t.Name, Host: t.Host, Params: t.Params},
			Pings:   t.Pings,
			Timeout: timeout,
			Step:    step,
			// Opaque hub-assigned identity tag; carried through the Outcome into the
			// buffered RoundReport so the hub can attribute a replayed round.
			Fingerprint: t.Fingerprint,
		})
	}
	return jobs, skipped
}
