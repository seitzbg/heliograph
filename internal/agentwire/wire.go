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
	// ID is the hub-computed stable storage identity for this target (model.Monitor.ID,
	// falling back to its path — see probe.Target.Key()). The agent treats it as opaque and
	// echoes it back as RoundReport.Target, so a target's history/alerts key stays stable
	// across a tree move even though Name (the display path) changes. Empty from a pre-id
	// hub; the agent then falls back to Name (old-hub compat).
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Probe       string            `json:"probe"`
	Host        string            `json:"host"`
	Params      map[string]string `json:"params,omitempty"`
	ProbeConfig map[string]string `json:"probe_config,omitempty"`
	StepMs      int64             `json:"step_ms"`
	Pings       int               `json:"pings"`
	// Fingerprint is the hub-computed measurement-identity hash for this target
	// (federation.Fingerprint over probe/host/params/pings/probe-config). The agent
	// treats it as opaque, carries it through the measurement, and echoes it on each
	// RoundReport so the hub can reject a round whose target was redefined since it was
	// measured. Empty from a pre-fingerprint agent; the hub accepts that transitionally.
	Fingerprint string `json:"fingerprint,omitempty"`
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
	Target string    `json:"target"`
	TS     string    `json:"ts"`    // RFC3339 / RFC3339Nano
	Pings  int       `json:"pings"` // N expected this round
	RTTs   []float64 `json:"rtts"`  // received RTTs in seconds (no nulls; loss = pings - len)
	Err    string    `json:"err,omitempty"`
	// Fingerprint echoes the assignment target's measurement-identity hash (opaque to
	// the agent). The hub recomputes it from the target's current config on ingest and
	// drops the round if they differ; empty is accepted transitionally (old agent).
	Fingerprint string  `json:"fingerprint,omitempty"`
	DurationMs  float64 `json:"duration_ms,omitempty"`
	// NTPOffsetMs and Stratum carry the NTP probe's companion clock stat — the latest offset
	// (milliseconds) and stratum for this target as measured at this vantage. The graphed series
	// still travels in RTTs (round-trip, or a signed offset in offset mode); these ride alongside
	// so the hub can show a remote NTP server's clock stat per vantage. Both are pointers and
	// omitempty: a non-NTP round, an unsynchronized clock, or a pre-stat agent omits them, and the
	// hub then shows no clock stat for that vantage rather than a stale or wrong one (CODE_REVIEW M2).
	NTPOffsetMs *float64 `json:"ntp_offset_ms,omitempty"`
	Stratum     *int     `json:"stratum,omitempty"`
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
