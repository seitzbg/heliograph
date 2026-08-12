package federation

import (
	"testing"
	"time"

	"github.com/seitzbg/heliograph/internal/model"
)

func mon(name string, vantages ...string) model.Monitor {
	return model.Monitor{Name: name, ProbeKind: "FPing", Host: "h", Pings: 3,
		Step: 60 * time.Second, Params: map[string]string{"a": "1"}, Vantages: vantages}
}

func TestAssignmentFor(t *testing.T) {
	mons := []model.Monitor{
		mon("a", "local"),
		mon("b", "nyc", "lon"),
		mon("c", "local", "nyc"),
	}
	names := func(ms []model.Monitor) []string {
		out := make([]string, len(ms))
		for i, m := range ms {
			out[i] = m.Name
		}
		return out
	}
	if got := names(AssignmentFor(mons, "local")); len(got) != 2 || got[0] != "a" || got[1] != "c" {
		t.Errorf("AssignmentFor(local) = %v, want [a c]", got)
	}
	if got := names(AssignmentFor(mons, "nyc")); len(got) != 2 || got[0] != "b" || got[1] != "c" {
		t.Errorf("AssignmentFor(nyc) = %v, want [b c]", got)
	}
	if got := AssignmentFor(mons, "tokyo"); len(got) != 0 {
		t.Errorf("AssignmentFor(tokyo) = %v, want empty", got)
	}
}

func monP(name, host string, params map[string]string, vantages ...string) model.Monitor {
	return model.Monitor{Name: name, ProbeKind: "FPing", Host: host, Pings: 3,
		Step: 60 * time.Second, Params: params, Vantages: vantages}
}

func TestConfigVersionStableAndSensitive(t *testing.T) {
	a := []model.Monitor{
		monP("a", "h", map[string]string{"port": "80", "path": "/x"}, "local"),
		monP("b", "h", map[string]string{"q": "1"}, "nyc"),
	}
	b := []model.Monitor{
		monP("b", "h", map[string]string{"q": "1"}, "nyc"),
		monP("a", "h", map[string]string{"path": "/x", "port": "80"}, "local"),
	}
	if ConfigVersion(a, nil) != ConfigVersion(b, nil) {
		t.Errorf("not order-independent:\n a=%s\n b=%s", ConfigVersion(a, nil), ConfigVersion(b, nil))
	}

	base := ConfigVersion(a, nil)
	mut := func(f func(x *model.Monitor)) string {
		x := monP("a", "h", map[string]string{"port": "80", "path": "/x"}, "local")
		f(&x)
		return ConfigVersion([]model.Monitor{x, a[1]}, nil)
	}
	if mut(func(x *model.Monitor) { x.Host = "h2" }) == base {
		t.Error("host change not reflected")
	}
	if mut(func(x *model.Monitor) { x.Pings = 4 }) == base {
		t.Error("pings change not reflected")
	}
	if mut(func(x *model.Monitor) { x.Step = 30 * time.Second }) == base {
		t.Error("step change not reflected")
	}
	if mut(func(x *model.Monitor) { x.ProbeKind = "HTTP" }) == base {
		t.Error("probe change not reflected")
	}
	if mut(func(x *model.Monitor) { x.Params = map[string]string{"port": "80", "path": "/y"} }) == base {
		t.Error("param value change not reflected")
	}

	c1 := []model.Monitor{monP("a", "h", map[string]string{"a": "b=c"}, "local")}
	c2 := []model.Monitor{monP("a", "h", map[string]string{"a=b": "c"}, "local")}
	if ConfigVersion(c1, nil) == ConfigVersion(c2, nil) {
		t.Error(`param separator collision: {"a":"b=c"} and {"a=b":"c"} hash equal`)
	}

	if v := base; len(v) < 8 || v[:7] != "sha256:" {
		t.Errorf("format = %q, want sha256: prefix", v)
	}
}

// A change to the effective probe-level config for a kind an assignment uses must
// bump that assignment's version (so agents re-fetch instead of getting a 304 with
// stale probe behavior), while a change to an unused kind's config must not
// (CODE_REVIEW #2 / P1-2).
func TestConfigVersionProbeConfigSensitive(t *testing.T) {
	a := []model.Monitor{monP("dns", "h", nil, "nyc")}
	a[0].ProbeKind = "DNS"

	base := ConfigVersion(a, map[string]map[string]string{"DNS": {"protocol": "udp"}})
	changed := ConfigVersion(a, map[string]map[string]string{"DNS": {"protocol": "tcp"}})
	if base == changed {
		t.Error("probe-level config change (DNS protocol udp->tcp) did not bump the version")
	}

	// A config block for a kind this assignment does not use must not affect its version.
	unrelated := ConfigVersion(a, map[string]map[string]string{
		"DNS":  {"protocol": "udp"},
		"HTTP": {"method": "HEAD"},
	})
	if base != unrelated {
		t.Error("unrelated probe kind's config leaked into the version")
	}
}

func TestFingerprintStableAndSensitive(t *testing.T) {
	m := monP("a", "h", map[string]string{"port": "80", "path": "/x"}, "local")
	cfg := map[string]string{"protocol": "udp"}
	base := Fingerprint(m, cfg)

	// Format.
	if len(base) < len("sha256:") || base[:len("sha256:")] != "sha256:" {
		t.Fatalf("Fingerprint format = %q, want sha256:<hex>", base)
	}

	// Deterministic and order-independent over Params and ProbeConfig maps.
	m2 := monP("a", "h", map[string]string{"path": "/x", "port": "80"}, "local")
	if Fingerprint(m2, map[string]string{"protocol": "udp"}) != base {
		t.Error("Fingerprint is not deterministic / order-independent over params")
	}

	// Each identity field changes the fingerprint.
	mut := func(f func(x *model.Monitor), c map[string]string) string {
		x := monP("a", "h", map[string]string{"port": "80", "path": "/x"}, "local")
		if f != nil {
			f(&x)
		}
		if c == nil {
			c = map[string]string{"protocol": "udp"}
		}
		return Fingerprint(x, c)
	}
	if mut(func(x *model.Monitor) { x.Host = "h2" }, nil) == base {
		t.Error("host change did not change the fingerprint")
	}
	if mut(func(x *model.Monitor) { x.Pings = 4 }, nil) == base {
		t.Error("pings change did not change the fingerprint")
	}
	if mut(func(x *model.Monitor) { x.ProbeKind = "HTTP" }, nil) == base {
		t.Error("probe kind change did not change the fingerprint")
	}
	if mut(func(x *model.Monitor) { x.Params = map[string]string{"port": "80", "path": "/y"} }, nil) == base {
		t.Error("params change did not change the fingerprint")
	}
	if mut(nil, map[string]string{"protocol": "tcp"}) == base {
		t.Error("probe-level config change (protocol udp->tcp) did not change the fingerprint")
	}

	// Name, step, vantages, and alerts do NOT change the fingerprint — they don't change
	// what a sample means (name is the lookup key; step/vantages are cadence/routing).
	if mut(func(x *model.Monitor) { x.Name = "b" }, nil) != base {
		t.Error("name leaked into the fingerprint (it must not — name is the lookup key)")
	}
	if mut(func(x *model.Monitor) { x.Step = 30 * time.Second }, nil) != base {
		t.Error("step leaked into the fingerprint")
	}
	if mut(func(x *model.Monitor) { x.Vantages = []string{"nyc", "lon"} }, nil) != base {
		t.Error("vantages leaked into the fingerprint")
	}

	// The JSON encoding must be unambiguous the same way ConfigVersion is: distinct
	// param maps that a bare-delimiter join would collide must differ.
	c1 := Fingerprint(monP("a", "h", map[string]string{"a": "b=c"}, "local"), nil)
	c2 := Fingerprint(monP("a", "h", map[string]string{"a=b": "c"}, "local"), nil)
	if c1 == c2 {
		t.Error("ambiguous encoding: {a:b=c} and {a=b:c} collided")
	}
}
