// Package probe defines the plugin contract for measurement backends and a
// registry so probes can be added without touching the core — the modern
// analog of SmokePing's lib/Smokeping/probes/*.pm plugin model (see codemap 03).
//
// A probe's whole job is to return the round-trip times it observed. The core
// (package sample) derives median, loss, and the centered smoke array. Loss is
// implicit: pings - len(Samples).
package probe

import (
	"context"
	"fmt"
	"sort"
)

// Target is one thing to measure.
type Target struct {
	Name   string            // display / path key
	Host   string            // hostname or IP
	Params map[string]string // per-target overrides (port, packetsize, ...)
}

// Result is what a probe returns for one round: the received RTTs in seconds.
// Order does not matter; lost pings are simply absent.
type Result struct {
	Samples []float64
}

// VarSpec describes one config variable a probe accepts. This is how each probe
// contributes its own schema fragment (replacing SmokePing's Config::Grammar
// _dyn per-probe schema mutation). A real build would emit JSON Schema from these.
type VarSpec struct {
	Doc       string
	Default   string
	Mandatory bool
	Scope     VarScope // probe-level or per-target
}

type VarScope int

const (
	ProbeVar  VarScope = iota // settable only on the probe instance
	TargetVar                 // also settable/overridable per target
)

// Probe is the plugin interface. Implementations must be safe for concurrent
// Measure calls (the scheduler runs many at once).
type Probe interface {
	// Name is the probe kind, e.g. "FPing", "TCPConnect".
	Name() string
	// Describe is the human label used on graphs (SmokePing's ProbeDesc).
	Describe() string
	// Schema returns the config variables this probe understands.
	Schema() map[string]VarSpec
	// Measure performs one round of `pings` measurements against t, honoring
	// ctx for cancellation/timeout. It returns the received RTTs (seconds).
	Measure(ctx context.Context, t Target, pings int) (Result, error)
}

// Factory builds a probe instance from its probe-level config.
type Factory func(cfg map[string]string) (Probe, error)

var registry = map[string]Factory{}

// Register makes a probe kind available by name. Called from probe packages'
// init() — the analog of SmokePing `require`-ing a probe module by name.
func Register(name string, f Factory) {
	if _, dup := registry[name]; dup {
		panic("probe: duplicate registration for " + name)
	}
	registry[name] = f
}

// New instantiates a registered probe kind with the given config.
func New(name string, cfg map[string]string) (Probe, error) {
	f, ok := registry[name]
	if !ok {
		return nil, fmt.Errorf("probe: unknown kind %q (registered: %v)", name, Registered())
	}
	return f(cfg)
}

// Registered lists the available probe kinds, sorted.
func Registered() []string {
	out := make([]string, 0, len(registry))
	for k := range registry {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Param returns a per-target param with a fallback default.
func (t Target) Param(key, def string) string {
	if t.Params != nil {
		if v, ok := t.Params[key]; ok && v != "" {
			return v
		}
	}
	return def
}
