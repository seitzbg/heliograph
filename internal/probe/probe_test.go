package probe

import (
	"context"
	"testing"
)

// stubProbe exercises JSONSchema with a mix of scopes, defaults, and a mandatory
// var, without depending on any real probe's binaries.
type stubProbe struct{}

func (stubProbe) Name() string     { return "Stub" }
func (stubProbe) Describe() string { return "stub probe" }
func (stubProbe) Schema() map[string]VarSpec {
	return map[string]VarSpec{
		"port":   {Doc: "TCP port", Default: "80", Scope: TargetVar},
		"lookup": {Doc: "name to resolve", Mandatory: true, Scope: TargetVar},
		"binary": {Doc: "path to helper", Default: "helper", Scope: ProbeVar},
		"seeded": {Doc: "mandatory but has a default", Mandatory: true, Default: "x", Scope: ProbeVar},
	}
}
func (stubProbe) Measure(context.Context, Target, int) (Result, error) { return Result{}, nil }

func TestJSONSchemaShape(t *testing.T) {
	s := JSONSchema(stubProbe{})
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

	probeCfg := s["probe_config"].(map[string]any)
	pprops := probeCfg["properties"].(map[string]any)
	if _, ok := pprops["binary"]; !ok {
		t.Errorf("probe config missing 'binary'")
	}
	// probe-scoped vars must not leak into target params and vice-versa.
	if _, leaked := tprops["binary"]; leaked {
		t.Errorf("probe-scoped 'binary' leaked into target params")
	}
	// 'seeded' is mandatory but has a default -> must NOT be required (matches
	// runtime validation, which treats a default as satisfying mandatory).
	if contains(toStrings(probeCfg["required"]), "seeded") {
		t.Errorf("'seeded' has a default and must not be required")
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
