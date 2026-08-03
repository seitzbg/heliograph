// Package model holds the configuration-side types. In a full build these are
// materialized from the YAML/DB target tree with inheritance (see codemap 02);
// here a Monitor is a flat leaf: which probe measures which host, how often.
package model

import "time"

// Monitor is one configured measurement (a leaf target).
type Monitor struct {
	Name      string            // display / path key, e.g. "Cloudflare DNS"
	ProbeKind string            // registered probe name, e.g. "FPing", "TCPConnect"
	Host      string            // hostname or IP
	Pings     int               // samples per round (N)
	Step      time.Duration     // polling interval
	Params    map[string]string // per-target probe params (port, packetsize, ...)
}
