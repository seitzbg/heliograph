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
	"slices"
	"sort"
	"strconv"
	"strings"
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
// _dyn per-probe schema mutation), and the single source both runtime validation
// and the published JSON Schema draw from — so a bad value is a loud config error
// rather than a silent runtime fallback.
type VarSpec struct {
	Doc       string
	Default   string
	Mandatory bool
	Scope     VarScope // probe-level or per-target
	// Value constraints (all optional). Kind + Enum are declarative and reflected
	// in the JSON Schema; Validate is an escape hatch for constraints a schema
	// can't express (e.g. "any valid DNS record type") and is enforced at runtime only.
	Kind     VarKind
	Enum     []string
	Validate func(string) error
}

// VarKind constrains a config value's form. Values are always strings (config is
// map[string]string), so these validate the string's shape.
type VarKind int

const (
	KindString VarKind = iota // any string (default)
	KindBool                  // "true" or "false"
	KindInt                   // a non-negative integer
	KindPort                  // an integer in 1..65535
)

// ValidateValue checks value against the spec's constraints (Kind, then Enum, then
// Validate). name is used in the error message. An unconstrained spec accepts anything.
func (s VarSpec) ValidateValue(name, value string) error {
	switch s.Kind {
	case KindBool:
		if value != "true" && value != "false" {
			return fmt.Errorf("%s must be true or false, got %q", name, value)
		}
	case KindInt:
		if n, err := strconv.Atoi(value); err != nil || n < 0 {
			return fmt.Errorf("%s must be a non-negative integer, got %q", name, value)
		}
	case KindPort:
		if n, err := strconv.Atoi(value); err != nil || n < 1 || n > 65535 {
			return fmt.Errorf("%s must be a port 1-65535, got %q", name, value)
		}
	}
	if len(s.Enum) > 0 && !slices.Contains(s.Enum, value) {
		return fmt.Errorf("%s must be one of %s, got %q", name, strings.Join(s.Enum, ", "), value)
	}
	if s.Validate != nil {
		return s.Validate(value)
	}
	return nil
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

// JSONSchema renders a probe's config variables as JSON Schema (draft 2020-12) —
// the same VarSpec source that drives runtime validation, emitted for docs and
// external validation. Probe-level vars (the `probes.<Kind>` block) and per-target
// vars (a target's `params`) configure different surfaces, so each gets its own
// schema. Config values are strings, so every property is typed "string".
func JSONSchema(p Probe) map[string]any {
	probeProps, targetProps := map[string]any{}, map[string]any{}
	var probeReq, targetReq []string
	for name, spec := range p.Schema() {
		prop := map[string]any{"type": "string"}
		if spec.Doc != "" {
			prop["description"] = spec.Doc
		}
		if spec.Default != "" {
			prop["default"] = spec.Default
		}
		// Reflect value constraints (config values are strings, so numeric kinds
		// become patterns; a Validate func can't be rendered — its Doc carries it).
		switch spec.Kind {
		case KindBool:
			prop["enum"] = []string{"true", "false"}
		case KindInt:
			prop["pattern"] = "^[0-9]+$"
		case KindPort:
			prop["pattern"] = "^[0-9]{1,5}$"
		}
		if len(spec.Enum) > 0 {
			prop["enum"] = spec.Enum
		}
		if spec.Scope == TargetVar {
			targetProps[name] = prop
			if spec.Mandatory && spec.Default == "" {
				targetReq = append(targetReq, name)
			}
		} else {
			probeProps[name] = prop
			if spec.Mandatory && spec.Default == "" {
				probeReq = append(probeReq, name)
			}
		}
	}
	return map[string]any{
		"name":          p.Name(),
		"describe":      p.Describe(),
		"probe_config":  schemaObject(probeProps, probeReq),
		"target_params": schemaObject(targetProps, targetReq),
	}
}

func schemaObject(props map[string]any, required []string) map[string]any {
	sort.Strings(required)
	obj := map[string]any{
		"$schema":              "https://json-schema.org/draft/2020-12/schema",
		"type":                 "object",
		"additionalProperties": false,
		"properties":           props,
	}
	if len(required) > 0 {
		obj["required"] = required
	}
	return obj
}

// AllSchemas returns JSONSchema for every registered probe kind that can be
// instantiated, sorted by name. A kind whose instance can't be built (e.g. a
// missing external binary) is skipped rather than failing the whole set.
func AllSchemas() []map[string]any {
	var out []map[string]any
	for _, kind := range Registered() {
		p, err := New(kind, nil)
		if err != nil {
			continue
		}
		out = append(out, JSONSchema(p))
	}
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
