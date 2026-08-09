// Package agentwire defines the JSON wire contract shared by the hub
// (internal/api) and the smoke-agent binary. Both sides import these types
// so the assignment and results formats can never drift between them.
package agentwire

// AssignmentTarget is one monitor target handed to a vantage agent.
//
// Params are per-target probe params (the target's `params:` block); ProbeConfig
// is the hub's effective probe-level config for this target's kind (the
// `probes.<Kind>` block) so a remote agent constructs the probe with the same
// settings the hub uses locally — e.g. DNS protocol tcp, HTTP method HEAD —
// instead of bare probe defaults (CODE_REVIEW #2 / P1-2).
type AssignmentTarget struct {
	Name        string            `json:"name"`
	Probe       string            `json:"probe"`
	Host        string            `json:"host"`
	Params      map[string]string `json:"params,omitempty"`
	ProbeConfig map[string]string `json:"probe_config,omitempty"`
	StepMs      int64             `json:"step_ms"`
	Pings       int               `json:"pings"`
}

// Assignment is the response body for GET /agent/v1/assignment: the full
// target list for the calling vantage, plus a config_version usable as an
// ETag.
type Assignment struct {
	Vantage       string             `json:"vantage"`
	ConfigVersion string             `json:"config_version"`
	Targets       []AssignmentTarget `json:"targets"`
}

// RoundReport is one measured round for one target, as submitted by an agent.
type RoundReport struct {
	Target     string    `json:"target"`
	TS         string    `json:"ts"`    // RFC3339 / RFC3339Nano
	Pings      int       `json:"pings"` // N expected this round
	RTTs       []float64 `json:"rtts"`  // received RTTs in seconds (no nulls; loss = pings - len)
	Err        string    `json:"err,omitempty"`
	DurationMs float64   `json:"duration_ms,omitempty"`
}

// ResultsRequest is the request body for POST /agent/v1/results.
type ResultsRequest struct {
	Results []RoundReport `json:"results"`
}

// Ingest limits shared by the hub (which enforces them) and the agent (which caps its flush
// batch to MaxResultsRounds). Keeping them in one place stops the agent from ever building a
// batch the hub is guaranteed to reject, which would otherwise wedge its flush loop retrying
// the same permanently-rejected batch forever (CODE_REVIEW #2).
const (
	MaxResultsRounds = 5000     // max rounds per POST /agent/v1/results
	MaxResultsBytes  = 16 << 20 // 16 MiB request-body cap
)

// ResultsResponse is the response body for POST /agent/v1/results.
type ResultsResponse struct {
	Accepted int `json:"accepted"`
	Dropped  int `json:"dropped"`
}
