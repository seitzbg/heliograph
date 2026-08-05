// Package config loads the YAML target tree and flattens it into a list of
// monitors, resolving inheritance down the tree (set `probe` once, children
// inherit) — the ergonomic core of SmokePing's config (see codemap 02). Each
// probe validates its own target params via its Schema(), the modern stand-in
// for SmokePing's per-probe dynamic grammar (_dyn).
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"smokeping-modern/internal/alert"
	"smokeping-modern/internal/model"
	"smokeping-modern/internal/probe"
)

// Duration is a time.Duration that unmarshals from a string like "60s".
type Duration time.Duration

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(v)
	return nil
}

type Config struct {
	Database Database                     `yaml:"database"`
	Probes   map[string]map[string]string `yaml:"probes"` // probe kind -> probe-level params
	Alerts   map[string]AlertDef          `yaml:"alerts"` // name -> alert definition
	Targets  *Node                        `yaml:"targets"`
}

// AlertDef is the YAML shape of one alert.
type AlertDef struct {
	Type        string   `yaml:"type"`    // "loss" | "rtt" | "matcher"
	Pattern     string   `yaml:"pattern"` // for loss/rtt: ">50%,>50%" or ">200,>200" (ms)
	Matcher     string   `yaml:"matcher"` // for matcher: "CheckLoss(l=50,x=3)"
	Comment     string   `yaml:"comment"`
	EdgeTrigger bool     `yaml:"edgetrigger"`
	To          []string `yaml:"to"`       // notifier names; defaults to ["log"]
	Priority    int      `yaml:"priority"` // 1 = highest; 0 = unset (never inhibited)
}

type Database struct {
	Step  Duration `yaml:"step"`
	Pings int      `yaml:"pings"`
}

// Node is one entry in the target tree. A node with a `host` becomes a monitor;
// any node may carry inheritable settings and children.
type Node struct {
	Probe    string            `yaml:"probe"`
	Host     string            `yaml:"host"`
	Title    string            `yaml:"title"`
	Pings    int               `yaml:"pings"`
	Step     Duration          `yaml:"step"`
	Params   map[string]string `yaml:"params"`
	Alerts   []string          `yaml:"alerts"`  // alert names; inherited down the tree
	Alertee  []string          `yaml:"alertee"` // extra notifier names; inherited down the tree
	Children map[string]*Node  `yaml:"children"`
}

// BuildAlerts compiles the alert definitions into runnable alerts.
func (c *Config) BuildAlerts() (map[string]*alert.Alert, error) {
	out := map[string]*alert.Alert{}
	for name, d := range c.Alerts {
		var m alert.Matcher
		var err error
		switch d.Type {
		case "loss":
			m, err = alert.ParsePattern("loss", d.Pattern)
		case "rtt":
			m, err = alert.ParsePattern("rtt", d.Pattern)
		case "matcher":
			m, err = alert.ParseMatcher(d.Matcher)
		default:
			err = fmt.Errorf("alert %q: unknown type %q (want loss|rtt|matcher)", name, d.Type)
		}
		if err != nil {
			return nil, fmt.Errorf("alert %q: %w", name, err)
		}
		to := d.To
		if len(to) == 0 {
			to = []string{"log"}
		}
		if d.Priority < 0 {
			return nil, fmt.Errorf("alert %q: priority must be >= 1 (or 0/unset), got %d", name, d.Priority)
		}
		out[name] = &alert.Alert{Name: name, Matcher: m, EdgeTrigger: d.EdgeTrigger, Comment: d.Comment, To: to, Priority: d.Priority}
	}
	return out, nil
}

// Load reads and parses a YAML config file.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	c, err := Parse(b)
	if err != nil {
		return nil, fmt.Errorf("config: parse %s: %w", path, err)
	}
	return c, nil
}

// Parse parses YAML bytes into a Config and applies defaults. Parsing is strict:
// an unknown field (a typo like `porbe:` or `databse:`) is a hard error rather
// than a silently ignored setting.
func Parse(b []byte) (*Config, error) {
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if c.Database.Pings == 0 {
		c.Database.Pings = 20
	}
	if c.Database.Step == 0 {
		c.Database.Step = Duration(60 * time.Second)
	}
	return &c, nil
}

type inherited struct {
	probe   string
	pings   int
	step    time.Duration
	params  map[string]string
	alerts  []string
	alertee []string
}

func mergeParams(parent, child map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range parent {
		out[k] = v
	}
	for k, v := range child {
		out[k] = v
	}
	return out
}

// Monitors flattens the tree into leaf monitors, applying inheritance and
// validating each against the target probe's schema. It returns all monitors it
// could resolve plus a combined error describing any invalid leaves.
func (c *Config) Monitors() ([]model.Monitor, error) {
	if c.Targets == nil {
		return nil, fmt.Errorf("config: no targets defined")
	}
	// One probe instance per kind (from probe-level config) to read schemas.
	schemas := map[string]map[string]probe.VarSpec{}
	schemaErr := map[string]error{}
	getSchema := func(kind string) (map[string]probe.VarSpec, error) {
		if s, ok := schemas[kind]; ok {
			return s, schemaErr[kind]
		}
		p, err := probe.New(kind, c.Probes[kind])
		if err != nil {
			schemas[kind], schemaErr[kind] = nil, err
			return nil, err
		}
		schemas[kind] = p.Schema()
		return schemas[kind], nil
	}

	var out []model.Monitor
	var problems []string

	// Validate probe-level config blocks (probes.<Kind>) against each probe's
	// schema, mirroring the additionalProperties:false published for probe_config.
	// A registered kind that can't be constructed here (e.g. missing binary) is
	// skipped, like target validation, since its schema is unavailable.
	for _, kind := range sortedProbeKinds(c.Probes) {
		if !slices.Contains(probe.Registered(), kind) {
			problems = append(problems, fmt.Sprintf("probes.%s: unknown probe kind", kind))
			continue
		}
		schema, err := getSchema(kind)
		if err != nil {
			continue
		}
		for _, name := range sortedStrKeys(c.Probes[kind]) {
			if _, ok := schema[name]; !ok {
				problems = append(problems, fmt.Sprintf("probes.%s: unknown param %q", kind, name))
			}
		}
	}

	var walk func(path string, n *Node, inh inherited)
	walk = func(path string, n *Node, inh inherited) {
		// A YAML entry with no value (`empty:`) decodes to a nil *Node. Report it as
		// a config problem instead of dereferencing it and panicking (which, in the
		// reload goroutine, would take the whole collector down).
		if n == nil {
			problems = append(problems, fmt.Sprintf("%s: empty node — give it a host and/or children, or remove it", path))
			return
		}
		// An explicit empty list ([]) clears an inherited value; an absent field
		// (nil slice) keeps inheriting. len()>0 would conflate the two.
		alerts := inh.alerts
		if n.Alerts != nil {
			alerts = n.Alerts
		}
		alertee := inh.alertee
		if n.Alertee != nil {
			alertee = n.Alertee
		}
		eff := inherited{
			probe:   firstNonEmpty(n.Probe, inh.probe),
			pings:   firstNonZero(n.Pings, inh.pings),
			step:    firstNonZeroDur(time.Duration(n.Step), inh.step),
			params:  mergeParams(inh.params, n.Params),
			alerts:  alerts,
			alertee: alertee,
		}
		if n.Host != "" {
			m := model.Monitor{
				Name: path, ProbeKind: eff.probe, Host: n.Host,
				Pings: eff.pings, Step: eff.step, Params: eff.params, Alerts: eff.alerts, Alertee: eff.alertee,
			}
			if err := validate(path, m, getSchema); err != nil {
				problems = append(problems, err.Error())
			} else {
				for _, an := range m.Alerts {
					if _, ok := c.Alerts[an]; !ok {
						problems = append(problems, fmt.Sprintf("%s: references undefined alert %q", path, an))
					}
				}
				out = append(out, m)
			}
		}
		for _, key := range sortedKeys(n.Children) {
			child := path + "/" + key
			if path == "" {
				child = key
			}
			walk(child, n.Children[key], eff)
		}
	}
	walk("", c.Targets, inherited{pings: c.Database.Pings, step: time.Duration(c.Database.Step)})

	if len(problems) > 0 {
		return out, fmt.Errorf("config: %d invalid target(s):\n  - %s", len(problems), strings.Join(problems, "\n  - "))
	}
	return out, nil
}

// MaxPings bounds the per-round sample count. The store persists pings as a
// PostgreSQL smallint, and an unbounded count would also allocate proportional
// slices per round — so reject anything absurd at config time.
const MaxPings = 10000

func validate(path string, m model.Monitor, getSchema func(string) (map[string]probe.VarSpec, error)) error {
	if m.ProbeKind == "" {
		return fmt.Errorf("%s: no probe set (and none inherited)", path)
	}
	if m.Pings < 1 || m.Pings > MaxPings {
		return fmt.Errorf("%s: pings must be between 1 and %d, got %d", path, MaxPings, m.Pings)
	}
	if m.Step <= 0 {
		return fmt.Errorf("%s: step must be positive, got %s", path, m.Step)
	}
	if !slices.Contains(probe.Registered(), m.ProbeKind) {
		return fmt.Errorf("%s: unknown probe kind %q", path, m.ProbeKind)
	}
	schema, err := getSchema(m.ProbeKind)
	if err != nil {
		// The kind is registered but couldn't be constructed here — typically its
		// external binary (fping, irtt, ...) isn't installed on this machine. That
		// must not fail config validation: the collector skips such targets at
		// runtime with a warning. We just can't check target vars without a schema.
		return nil
	}
	for name, spec := range schema {
		if spec.Scope == probe.TargetVar && spec.Mandatory {
			if _, ok := m.Params[name]; !ok && spec.Default == "" {
				return fmt.Errorf("%s (%s): missing required param %q", path, m.ProbeKind, name)
			}
		}
	}
	// Enforce the published schema's additionalProperties:false and scope on target
	// params: an unknown name is a typo; a probe-scoped var set per target would be
	// silently ignored at runtime. Both should be loud config errors.
	for name := range m.Params {
		spec, ok := schema[name]
		if !ok {
			return fmt.Errorf("%s (%s): unknown target param %q", path, m.ProbeKind, name)
		}
		if spec.Scope != probe.TargetVar {
			return fmt.Errorf("%s (%s): param %q is probe-level (set it under probes.%s), not per target", path, m.ProbeKind, name, m.ProbeKind)
		}
	}
	return nil
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
func firstNonZero(a, b int) int {
	if a != 0 {
		return a
	}
	return b
}
func firstNonZeroDur(a, b time.Duration) time.Duration {
	if a != 0 {
		return a
	}
	return b
}
func sortedKeys(m map[string]*Node) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
func sortedProbeKinds(m map[string]map[string]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
func sortedStrKeys(m map[string]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
