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
	"time"
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
	KindString      VarKind = iota // any string (default)
	KindBool                       // "true" or "false"
	KindInt                        // a non-negative integer
	KindPositiveInt                // an integer >= 1 (e.g. a send interval / period in ms)
	KindPort                       // an integer in 1..65535
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
	case KindPositiveInt:
		if n, err := strconv.Atoi(value); err != nil || n < 1 {
			return fmt.Errorf("%s must be a positive integer, got %q", name, value)
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
// Measure calls (the scheduler runs many at once). The config schema and human
// label live in the registry (see Register), not on the instance, so they are
// available for validation and docs without constructing the probe — which for an
// external-binary probe (fping, irtt) can fail when the binary is absent.
type Probe interface {
	// Name is the probe kind, e.g. "FPing", "TCPConnect".
	Name() string
	// Measure performs one round of `pings` measurements against t, honoring
	// ctx for cancellation/timeout. It returns the received RTTs (seconds).
	Measure(ctx context.Context, t Target, pings int) (Result, error)
}

// Factory builds a probe instance from its probe-level config.
type Factory func(cfg map[string]string) (Probe, error)

// registration holds everything a kind exposes. schema and describe are static
// (independent of an instance), so config validation and the schema docs work even
// when the probe can't be constructed (missing external binary).
type registration struct {
	factory  Factory
	describe string
	schema   map[string]VarSpec
}

var registry = map[string]registration{}

// Register makes a probe kind available by name. Called from probe packages'
// init() — the analog of SmokePing `require`-ing a probe module by name. describe is
// the human label; schema is the config variables the kind understands.
func Register(name, describe string, schema map[string]VarSpec, f Factory) {
	if _, dup := registry[name]; dup {
		panic("probe: duplicate registration for " + name)
	}
	registry[name] = registration{factory: f, describe: describe, schema: schema}
}

// New instantiates a registered probe kind with the given config.
func New(name string, cfg map[string]string) (Probe, error) {
	r, ok := registry[name]
	if !ok {
		return nil, fmt.Errorf("probe: unknown kind %q (registered: %v)", name, Registered())
	}
	return r.factory(cfg)
}

// SchemaOf returns a kind's static config schema without constructing it, so config
// validation can check target params even when the probe's external binary is absent.
// The bool is false for an unregistered kind.
func SchemaOf(name string) (map[string]VarSpec, bool) {
	r, ok := registry[name]
	return r.schema, ok
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
func JSONSchema(name, describe string, schema map[string]VarSpec) map[string]any {
	probeProps, targetProps := map[string]any{}, map[string]any{}
	var probeReq, targetReq []string
	for varName, spec := range schema {
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
		case KindPositiveInt:
			prop["pattern"] = "^[1-9][0-9]*$"
		case KindPort:
			prop["pattern"] = "^[0-9]{1,5}$"
		}
		if len(spec.Enum) > 0 {
			prop["enum"] = spec.Enum
		}
		// probe_config accepts every var — probe-only vars, plus target vars set at
		// probe level as a tree-wide default (the runtime reads both from the
		// probes.<Kind> block). target_params accepts only target-scoped vars. This
		// matches what config validation enforces (#12 schema/runtime parity).
		probeProps[varName] = prop
		if spec.Mandatory && spec.Default == "" && spec.Scope != TargetVar {
			probeReq = append(probeReq, varName) // a target var can be satisfied per target instead
		}
		if spec.Scope == TargetVar {
			targetProps[varName] = prop
			if spec.Mandatory && spec.Default == "" {
				targetReq = append(targetReq, varName)
			}
		}
	}
	return map[string]any{
		"name":          name,
		"describe":      describe,
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

// AllSchemas returns JSONSchema for every registered probe kind, sorted by name.
// It reads the static registry (schema + describe), so a kind whose external binary
// is absent still appears — the schema no longer depends on constructing the probe.
func AllSchemas() []map[string]any {
	var out []map[string]any
	for _, kind := range Registered() {
		r := registry[kind]
		out = append(out, JSONSchema(kind, r.describe, r.schema))
	}
	return out
}

// PerPingBudget returns the per-ping timeout a sequential probe (TCPConnect/HTTP/DNS/
// SSH) should give each of its `pings` attempts: an even slice of the round budget
// left on ctx. The scheduler sets ctx's deadline to min(timeout*pings, step), so this
// is min(timeout, step/pings) — never more than the configured -timeout. Callers
// compute it ONCE before the ping loop so every attempt gets the same fixed cap: a
// fast early attempt must not hand its unused time to a later one (which would let a
// later ping run past -timeout). Returns 0 when ctx has no deadline (Pings==1 callers,
// tests), meaning "no per-ping bound".
func PerPingBudget(ctx context.Context, pings int) time.Duration {
	if pings < 1 {
		pings = 1
	}
	dl, ok := ctx.Deadline()
	if !ok {
		return 0
	}
	return time.Until(dl) / time.Duration(pings)
}

// AttemptContext derives the context for one ping of a sequential probe, bounded to
// perPing (from PerPingBudget). Without it, all pings share the one round deadline, so
// a hung or blackholed target lets the first connect consume the whole budget and the
// loop bails after a single attempt — reporting one lost ping instead of N and tying
// up a worker for the entire round. The parent's deadline still caps the total, so the
// last attempt can't overrun the round. perPing<=0 inherits the parent unbounded. The
// caller must always cancel the returned context (defer cancel()).
func AttemptContext(parent context.Context, perPing time.Duration) (context.Context, context.CancelFunc) {
	if perPing <= 0 {
		return context.WithCancel(parent)
	}
	return context.WithTimeout(parent, perPing)
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
