// Package config loads the YAML target tree and flattens it into a list of
// monitors, resolving inheritance down the tree (set `probe` once, children
// inherit) — the ergonomic core of SmokePing's config (see codemap 02). Each
// probe validates its own target params via its Schema(), the modern stand-in
// for SmokePing's per-probe dynamic grammar (_dyn).
package config

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

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
	Targets  *Node                        `yaml:"targets"`
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
	Children map[string]*Node  `yaml:"children"`
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

// Parse parses YAML bytes into a Config and applies defaults.
func Parse(b []byte) (*Config, error) {
	var c Config
	if err := yaml.Unmarshal(b, &c); err != nil {
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
	probe  string
	pings  int
	step   time.Duration
	params map[string]string
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

	var walk func(path string, n *Node, inh inherited)
	walk = func(path string, n *Node, inh inherited) {
		eff := inherited{
			probe:  firstNonEmpty(n.Probe, inh.probe),
			pings:  firstNonZero(n.Pings, inh.pings),
			step:   firstNonZeroDur(time.Duration(n.Step), inh.step),
			params: mergeParams(inh.params, n.Params),
		}
		if n.Host != "" {
			m := model.Monitor{
				Name: path, ProbeKind: eff.probe, Host: n.Host,
				Pings: eff.pings, Step: eff.step, Params: eff.params,
			}
			if err := validate(path, m, getSchema); err != nil {
				problems = append(problems, err.Error())
			} else {
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

func validate(path string, m model.Monitor, getSchema func(string) (map[string]probe.VarSpec, error)) error {
	if m.ProbeKind == "" {
		return fmt.Errorf("%s: no probe set (and none inherited)", path)
	}
	schema, err := getSchema(m.ProbeKind)
	if err != nil {
		return fmt.Errorf("%s: %v", path, err)
	}
	for name, spec := range schema {
		if spec.Scope == probe.TargetVar && spec.Mandatory {
			if _, ok := m.Params[name]; !ok && spec.Default == "" {
				return fmt.Errorf("%s (%s): missing required param %q", path, m.ProbeKind, name)
			}
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
