package probe_test

import (
	"testing"

	"smokeping-modern/internal/probe"

	// register the real probe kinds so their schemas can be inspected
	_ "smokeping-modern/internal/probe/dns"
	_ "smokeping-modern/internal/probe/fping"
	_ "smokeping-modern/internal/probe/httpprobe"
	_ "smokeping-modern/internal/probe/irttprobe"
	_ "smokeping-modern/internal/probe/pingprobe"
	_ "smokeping-modern/internal/probe/sshprobe"
	_ "smokeping-modern/internal/probe/tcpconnect"
)

// Every probe's own default value must satisfy its own constraint — otherwise a
// user who omits the param gets a value the validator would reject (author drift).
// Schemas come from the registry, so this checks every probe including those whose
// external binary is absent here (#12).
func TestProbeSchemaDefaultsValid(t *testing.T) {
	for _, kind := range probe.Registered() {
		schema, ok := probe.SchemaOf(kind)
		if !ok {
			t.Errorf("%s registered but has no schema", kind)
			continue
		}
		for name, spec := range schema {
			if spec.Default == "" {
				continue
			}
			if err := spec.ValidateValue(name, spec.Default); err != nil {
				t.Errorf("%s: default for %q (%q) fails its own constraint: %v", kind, name, spec.Default, err)
			}
		}
	}
}

// The params the review flagged for silent runtime fallback must now carry a
// constraint (so a bad value is a loud config error, not a silent default).
func TestConstrainedParamsHaveConstraints(t *testing.T) {
	specOf := func(kind, param string) probe.VarSpec {
		schema, ok := probe.SchemaOf(kind)
		if !ok {
			t.Fatalf("%s not registered", kind)
		}
		s, ok := schema[param]
		if !ok {
			t.Fatalf("%s has no %q param", kind, param)
		}
		return s
	}
	if s := specOf("HTTP", "insecure_ssl"); s.Kind != probe.KindBool {
		t.Errorf("HTTP insecure_ssl Kind = %v, want KindBool", s.Kind)
	}
	if s := specOf("DNS", "protocol"); len(s.Enum) == 0 {
		t.Errorf("DNS protocol should be an enum")
	}
	if s := specOf("DNS", "recordtype"); s.Validate == nil {
		t.Errorf("DNS recordtype should have a Validate (any valid DNS type)")
	} else if err := s.Validate("AAAA"); err != nil {
		t.Errorf("DNS recordtype should accept AAAA: %v", err)
	} else if s.Validate("NOTATYPE") == nil {
		t.Errorf("DNS recordtype should reject NOTATYPE")
	}
	for _, kind := range []string{"TCPConnect", "DNS", "SSH", "IRTT"} {
		if s := specOf(kind, "port"); s.Kind != probe.KindPort {
			t.Errorf("%s port Kind = %v, want KindPort", kind, s.Kind)
		}
	}
}
