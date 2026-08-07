package federation

import (
	"testing"
	"time"

	"smokeping-modern/internal/model"
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
	if ConfigVersion(a) != ConfigVersion(b) {
		t.Errorf("not order-independent:\n a=%s\n b=%s", ConfigVersion(a), ConfigVersion(b))
	}

	base := ConfigVersion(a)
	mut := func(f func(x *model.Monitor)) string {
		x := monP("a", "h", map[string]string{"port": "80", "path": "/x"}, "local")
		f(&x)
		return ConfigVersion([]model.Monitor{x, a[1]})
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
	if ConfigVersion(c1) == ConfigVersion(c2) {
		t.Error(`param separator collision: {"a":"b=c"} and {"a=b":"c"} hash equal`)
	}

	if v := base; len(v) < 8 || v[:7] != "sha256:" {
		t.Errorf("format = %q, want sha256: prefix", v)
	}
}
