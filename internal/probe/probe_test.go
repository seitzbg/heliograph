package probe

import (
	"context"
	"testing"
	"time"
)

// PerPingBudget must hand each ping an even, FIXED share of the round budget
// (budget/pings) — never more than the configured -timeout — so a hung target is
// probed all N times and no single ping runs past its share. It is the shared bound
// every sequential probe (TCPConnect/HTTP/DNS/SSH) applies uniformly across the loop.
func TestPerPingBudgetFixedEvenShare(t *testing.T) {
	const budget = 900 * time.Millisecond
	parent, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()

	// 3 pings -> ~1/3 of the budget each, and it must not depend on which ping it is:
	// a fast early ping cannot inflate a later ping's share (that is the whole point).
	share := PerPingBudget(parent, 3)
	if share < 250*time.Millisecond || share > 300*time.Millisecond {
		t.Fatalf("per-ping share = %s, want ~300ms (budget/pings)", share)
	}

	// The attempt context carries that fixed cap, never past the parent deadline.
	actx, ac := AttemptContext(parent, share)
	defer ac()
	dl, ok := actx.Deadline()
	if !ok {
		t.Fatal("attempt context should carry a deadline when perPing > 0")
	}
	if d := time.Until(dl); d > share+20*time.Millisecond {
		t.Fatalf("attempt deadline %s exceeds the per-ping cap %s", d, share)
	}
	if pdl, _ := parent.Deadline(); dl.After(pdl) {
		t.Fatalf("attempt deadline %s is past the parent deadline %s", dl, pdl)
	}
}

// Without a parent deadline (Pings==1 callers, tests) there is no per-ping bound and
// an attempt inherits the parent unbounded, preserving those call sites' behavior.
func TestPerPingBudgetNoParentDeadline(t *testing.T) {
	if d := PerPingBudget(context.Background(), 3); d != 0 {
		t.Fatalf("no parent deadline should yield a 0 (unbounded) per-ping budget, got %s", d)
	}
	actx, cancel := AttemptContext(context.Background(), 0)
	defer cancel()
	if _, ok := actx.Deadline(); ok {
		t.Fatal("attempt context must not invent a deadline when perPing <= 0")
	}
}

// stubSchema exercises JSONSchema with a mix of scopes, defaults, and a mandatory var.
var stubSchema = map[string]VarSpec{
	"port":   {Doc: "TCP port", Default: "80", Scope: TargetVar},
	"lookup": {Doc: "name to resolve", Mandatory: true, Scope: TargetVar},
	"binary": {Doc: "path to helper", Default: "helper", Scope: ProbeVar},
	"seeded": {Doc: "mandatory but has a default", Mandatory: true, Default: "x", Scope: ProbeVar},
}

func TestJSONSchemaShape(t *testing.T) {
	s := JSONSchema("Stub", "stub probe", stubSchema)
	if s["name"] != "Stub" || s["describe"] != "stub probe" {
		t.Fatalf("name/describe wrong: %v", s)
	}

	target := s["target_params"].(map[string]any)
	if target["type"] != "object" || target["additionalProperties"] != false {
		t.Errorf("target schema not a closed object: %v", target)
	}
	tprops := target["properties"].(map[string]any)
	if _, ok := tprops["port"]; !ok {
		t.Errorf("target params missing 'port'")
	}
	port := tprops["port"].(map[string]any)
	if port["type"] != "string" || port["default"] != "80" || port["description"] != "TCP port" {
		t.Errorf("port property wrong: %v", port)
	}
	// 'lookup' is mandatory with no default -> required; 'port' has a default -> not.
	req := toStrings(target["required"])
	if !contains(req, "lookup") || contains(req, "port") {
		t.Errorf("target required = %v, want [lookup] only", req)
	}
	// Probe-scoped vars must NOT appear in target params.
	if _, leaked := tprops["binary"]; leaked {
		t.Errorf("probe-scoped 'binary' leaked into target params")
	}

	probeCfg := s["probe_config"].(map[string]any)
	pprops := probeCfg["properties"].(map[string]any)
	// probe_config accepts every var: probe-scoped AND target-scoped (settable at
	// probe level as a tree-wide default) — matching what runtime validation accepts.
	for _, name := range []string{"binary", "seeded", "port", "lookup"} {
		if _, ok := pprops[name]; !ok {
			t.Errorf("probe_config missing %q (should list all vars)", name)
		}
	}
	// But required at probe level = mandatory probe-scoped vars with no default only.
	// 'seeded' has a default; 'lookup' is target-scoped (satisfiable per target); neither
	// is required in probe_config.
	preq := toStrings(probeCfg["required"])
	if contains(preq, "seeded") || contains(preq, "lookup") || contains(preq, "port") {
		t.Errorf("probe_config required = %v, want none (no mandatory-no-default probe var)", preq)
	}
}

func toStrings(v any) []string {
	if v == nil {
		return nil
	}
	in := v.([]string)
	return in
}

func contains(s []string, want string) bool {
	for _, v := range s {
		if v == want {
			return true
		}
	}
	return false
}

func TestVarSpecValidateValue(t *testing.T) {
	errType := func(msg string) error { return &vsErr{msg} }
	cases := []struct {
		name  string
		spec  VarSpec
		value string
		ok    bool
	}{
		{"bool true", VarSpec{Kind: KindBool}, "true", true},
		{"bool false", VarSpec{Kind: KindBool}, "false", true},
		{"bool typo", VarSpec{Kind: KindBool}, "ture", false},
		{"port ok", VarSpec{Kind: KindPort}, "443", true},
		{"port low", VarSpec{Kind: KindPort}, "0", false},
		{"port high", VarSpec{Kind: KindPort}, "70000", false},
		{"port nan", VarSpec{Kind: KindPort}, "https", false},
		{"int ok", VarSpec{Kind: KindInt}, "50", true},
		{"int zero ok", VarSpec{Kind: KindInt}, "0", true},
		{"int neg", VarSpec{Kind: KindInt}, "-1", false},
		{"int nan", VarSpec{Kind: KindInt}, "x", false},
		{"posint ok", VarSpec{Kind: KindPositiveInt}, "20", true},
		{"posint one", VarSpec{Kind: KindPositiveInt}, "1", true},
		{"posint zero", VarSpec{Kind: KindPositiveInt}, "0", false},
		{"posint neg", VarSpec{Kind: KindPositiveInt}, "-5", false},
		{"posint nan", VarSpec{Kind: KindPositiveInt}, "x", false},
		{"enum ok", VarSpec{Enum: []string{"udp", "tcp"}}, "tcp", true},
		{"enum bad", VarSpec{Enum: []string{"udp", "tcp"}}, "sctp", false},
		{"validate ok", VarSpec{Validate: func(v string) error {
			if v == "A" {
				return nil
			}
			return errType("bad")
		}}, "A", true},
		{"validate bad", VarSpec{Validate: func(v string) error { return errType("bad") }}, "Z", false},
		{"string unconstrained", VarSpec{}, "anything", true},
	}
	for _, c := range cases {
		err := c.spec.ValidateValue("v", c.value)
		if (err == nil) != c.ok {
			t.Errorf("%s: ValidateValue(%q) err=%v, want ok=%v", c.name, c.value, err, c.ok)
		}
	}
}

type vsErr struct{ s string }

func (e *vsErr) Error() string { return e.s }
