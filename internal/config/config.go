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
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"smokeping-modern/internal/alert"
	"smokeping-modern/internal/model"
	"smokeping-modern/internal/probe"
	"smokeping-modern/internal/store"
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
	Alerts   []string          `yaml:"alerts"`   // alert names; inherited down the tree
	Alertee  []string          `yaml:"alertee"`  // extra notifier names; inherited down the tree
	Vantages []string          `yaml:"vantages"` // vantage points that probe this target; inherited
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

// Load reads and parses a single YAML config file.
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

// LoadPath loads config from `path`: a single YAML file (via Load) or, if path is
// a directory, from default.yaml + conf.d/*.yaml within it (via LoadDir). The
// collector and the SIGHUP reload both go through here, so either layout works.
func LoadPath(path string) (*Config, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if fi.IsDir() {
		return LoadDir(path)
	}
	return Load(path)
}

// decode strictly parses YAML bytes into a Config WITHOUT applying defaults. Strict
// means an unknown field (a typo like `porbe:` or `databse:`) is a hard error, not
// a silently ignored setting. Keeping defaults out lets a caller tell "unset" from
// "set to the default value" — which LoadDir needs to reject `database` in a fragment.
func decode(b []byte) (*Config, error) {
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return &c, nil
}

// Parse parses YAML bytes into a Config and applies defaults (pings 20, step 60s).
func Parse(b []byte) (*Config, error) {
	c, err := decode(b)
	if err != nil {
		return nil, err
	}
	if c.Database.Pings == 0 {
		c.Database.Pings = 20
	}
	if c.Database.Step == 0 {
		c.Database.Step = Duration(60 * time.Second)
	}
	return c, nil
}

// LoadDir loads config from a directory: default.yaml (the base — database, probes,
// alerts, tree-wide target defaults) plus conf.d/*.{yaml,yml} in sorted filename
// order. Fragments are concatenated, not merged: each contributes top-level target
// branches under the root, inheriting default.yaml's tree-wide defaults. A fragment
// may contain ONLY targets.children — setting database/probes/alerts or tree-wide
// targets:-node fields in a fragment, or defining the same top-level branch twice,
// is a hard error (the loud-config-errors philosophy; see receiving-code-review).
func LoadDir(dir string) (*Config, error) {
	// Base from default.yaml if present; otherwise an empty config with defaults.
	base := &Config{}
	defPath := filepath.Join(dir, "default.yaml")
	defExists := false
	if b, err := os.ReadFile(defPath); err == nil {
		defExists = true
		if base, err = Parse(b); err != nil {
			return nil, fmt.Errorf("config: parse %s: %w", defPath, err)
		}
	} else if errors.Is(err, fs.ErrNotExist) {
		base, _ = Parse(nil) // defaults only
	} else {
		return nil, err
	}
	if base.Targets == nil {
		base.Targets = &Node{}
	}
	if base.Targets.Children == nil {
		base.Targets.Children = map[string]*Node{}
	}

	// origin tracks which file defined each top-level branch, for the dup-error message.
	origin := map[string]string{}
	for k := range base.Targets.Children {
		origin[k] = "default.yaml"
	}

	// Collect conf.d/*.{yaml,yml} in sorted order (the 10-/20- prefix idiom).
	confd := filepath.Join(dir, "conf.d")
	entries, err := os.ReadDir(confd)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	var files []string
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || strings.HasPrefix(n, ".") {
			continue
		}
		if strings.HasSuffix(n, ".yaml") || strings.HasSuffix(n, ".yml") {
			files = append(files, n)
		}
	}
	sort.Strings(files)

	if !defExists && len(files) == 0 {
		return nil, fmt.Errorf("config: %s has no default.yaml and no conf.d/*.yaml", dir)
	}

	for _, n := range files {
		b, err := os.ReadFile(filepath.Join(confd, n))
		if err != nil {
			return nil, err
		}
		frag, err := decode(b) // no defaults: so an unset database stays zero
		if err != nil {
			return nil, fmt.Errorf("config: parse conf.d/%s: %w", n, err)
		}
		if err := validateFragment(n, frag); err != nil {
			return nil, err
		}
		if frag.Targets == nil {
			continue
		}
		for _, key := range sortedKeys(frag.Targets.Children) {
			if prev, dup := origin[key]; dup {
				return nil, fmt.Errorf("config: duplicate top-level target %q in conf.d/%s (already defined in %s)", key, n, prev)
			}
			origin[key] = "conf.d/" + n
			base.Targets.Children[key] = frag.Targets.Children[key]
		}
	}
	return base, nil
}

// validateFragment enforces that a conf.d fragment carries only target branches:
// database / probes / alerts and any tree-wide targets:-node fields belong in
// default.yaml. name is the fragment's filename (for the error message).
func validateFragment(name string, c *Config) error {
	where := "conf.d/" + name
	if c.Database.Pings != 0 || c.Database.Step != 0 {
		return fmt.Errorf("config: %s: `database` must be set in default.yaml, not in a fragment", where)
	}
	if len(c.Probes) > 0 {
		return fmt.Errorf("config: %s: `probes` must be set in default.yaml, not in a fragment", where)
	}
	if len(c.Alerts) > 0 {
		return fmt.Errorf("config: %s: `alerts` must be set in default.yaml, not in a fragment", where)
	}
	if t := c.Targets; t != nil {
		if t.Probe != "" || t.Host != "" || t.Title != "" || t.Pings != 0 || t.Step != 0 || t.Params != nil || t.Alerts != nil || t.Alertee != nil || t.Vantages != nil {
			return fmt.Errorf("config: %s: `targets:` may contain only `children` in a fragment (tree-wide defaults belong in default.yaml)", where)
		}
	}
	return nil
}

type inherited struct {
	probe    string
	pings    int
	step     time.Duration
	params   map[string]string
	alerts   []string
	alertee  []string
	vantages []string
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
	// Schemas come from the registry, not a constructed probe instance, so target and
	// probe-level params are validated even when a probe's external binary (fping,
	// irtt) is absent — a config lint no longer silently skips those checks (#12).
	getSchema := func(kind string) (map[string]probe.VarSpec, error) {
		if s, ok := probe.SchemaOf(kind); ok {
			return s, nil
		}
		return nil, fmt.Errorf("unknown probe kind %q", kind)
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
			spec, ok := schema[name]
			if !ok {
				problems = append(problems, fmt.Sprintf("probes.%s: unknown param %q", kind, name))
				continue
			}
			if err := spec.ValidateValue(name, c.Probes[kind][name]); err != nil {
				problems = append(problems, fmt.Sprintf("probes.%s: %v", kind, err))
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
		// A non-nil but empty leaf (`x: {}`, or a node that sets only defaults and has
		// no host and no children) contributes no monitor and no branch — it is dead
		// config, silently ignored before. Reject it like the nil-node case (#12).
		if n.Host == "" && len(n.Children) == 0 {
			where := path
			if where == "" {
				where = "targets"
			}
			problems = append(problems, fmt.Sprintf("%s: empty node — give it a host and/or children, or remove it", where))
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
		vantages := inh.vantages
		if n.Vantages != nil {
			vantages = n.Vantages
		}
		eff := inherited{
			probe:    firstNonEmpty(n.Probe, inh.probe),
			pings:    firstNonZero(n.Pings, inh.pings),
			step:     firstNonZeroDur(time.Duration(n.Step), inh.step),
			params:   mergeParams(inh.params, n.Params),
			alerts:   alerts,
			alertee:  alertee,
			vantages: vantages,
		}
		if n.Host != "" {
			m := model.Monitor{
				Name: path, ProbeKind: eff.probe, Host: n.Host,
				Pings: eff.pings, Step: eff.step, Params: eff.params,
				Alerts: eff.alerts, Alertee: eff.alertee, Vantages: eff.vantages,
			}
			if err := validate(path, m, getSchema); err != nil {
				problems = append(problems, err.Error())
			} else {
				for _, an := range m.Alerts {
					if _, ok := c.Alerts[an]; !ok {
						problems = append(problems, fmt.Sprintf("%s: references undefined alert %q", path, an))
					}
				}
				if len(m.Vantages) == 0 {
					problems = append(problems, fmt.Sprintf("%s: no vantages — an explicit `vantages: []`? every target needs at least one vantage (default is [%s])", path, store.DefaultVantage))
				}
				for _, v := range m.Vantages {
					if v == "" {
						problems = append(problems, fmt.Sprintf("%s: vantages contains a blank entry", path))
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
	walk("", c.Targets, inherited{pings: c.Database.Pings, step: time.Duration(c.Database.Step), vantages: []string{store.DefaultVantage}})

	// The target name is the identity key for the scheduler, store, alert engine and
	// planner. Two config paths that flatten to the same name (a key containing "/",
	// or a host on the root -> empty name) would silently merge two targets, so reject
	// empties and duplicates before building the runtime (CODE_REVIEW #6).
	seenName := make(map[string]bool, len(out))
	for _, m := range out {
		if m.Name == "" {
			problems = append(problems, "a target has an empty name (a host on the root node? give it a child key instead)")
			continue
		}
		if seenName[m.Name] {
			problems = append(problems, fmt.Sprintf("%s: duplicate target name — two config paths collapse to the same identity (avoid \"/\" in a node key)", m.Name))
		}
		seenName[m.Name] = true
	}

	if len(problems) > 0 {
		return out, fmt.Errorf("config: %d invalid target(s):\n  - %s", len(problems), strings.Join(problems, "\n  - "))
	}
	return out, nil
}

// MaxPings bounds the per-round sample count. The store persists pings as a
// PostgreSQL smallint, and an unbounded count would also allocate proportional
// slices per round — so reject anything absurd at config time.
const MaxPings = 10000

// MinStep is the smallest allowed polling interval. The API serializes each round's
// timestamp at whole-second resolution, so a sub-second cadence would emit two rounds
// with the same timestamp — ambiguous on the graph X-axis and to the incremental grid
// watermark. One second is also a sane floor on collector/DB load (CODE_REVIEW #9).
const MinStep = time.Second

func validate(path string, m model.Monitor, getSchema func(string) (map[string]probe.VarSpec, error)) error {
	if m.ProbeKind == "" {
		return fmt.Errorf("%s: no probe set (and none inherited)", path)
	}
	if m.Pings < 1 || m.Pings > MaxPings {
		return fmt.Errorf("%s: pings must be between 1 and %d, got %d", path, MaxPings, m.Pings)
	}
	if m.Step < MinStep {
		return fmt.Errorf("%s: step must be at least %s (sub-second cadences make whole-second timestamps ambiguous), got %s", path, MinStep, m.Step)
	}
	if !slices.Contains(probe.Registered(), m.ProbeKind) {
		return fmt.Errorf("%s: unknown probe kind %q", path, m.ProbeKind)
	}
	schema, err := getSchema(m.ProbeKind)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
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
		if err := spec.ValidateValue(name, m.Params[name]); err != nil {
			return fmt.Errorf("%s (%s): %w", path, m.ProbeKind, err)
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
