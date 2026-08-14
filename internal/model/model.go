// Package model holds the configuration-side types. In a full build these are
// materialized from the YAML/DB target tree with inheritance (see codemap 02);
// here a Monitor is a flat leaf: which probe measures which host, how often.
package model

import "time"

// Monitor is one configured measurement (a leaf target).
type Monitor struct {
	Name      string            // display / path key, e.g. "Cloudflare DNS"
	Title     string            // optional display-name override for the graph header (falls back to Name)
	ProbeKind string            // registered probe name, e.g. "FPing", "TCPConnect"
	Host      string            // hostname or IP (the probe target)
	IP        string            // optional pinned IP shown in the title (display-only; probe still hits Host)
	Pings     int               // samples per round (N)
	Step      time.Duration     // polling interval
	Params    map[string]string // per-target probe params (port, packetsize, ...)
	Alerts    []string          // names of alerts to evaluate for this target
	Alertee   []string          // extra notifier names for this target's alert events
	Vantages  []string          // vantage points that probe this target; inherited, default [local]
}
