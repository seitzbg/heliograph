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

func TestConfigVersionStableAndSensitive(t *testing.T) {
	a := []model.Monitor{mon("a", "local"), mon("b", "nyc")}
	// Same content, different slice order + different Params map insertion order -> same version.
	b := []model.Monitor{
		{Name: "b", ProbeKind: "FPing", Host: "h", Pings: 3, Step: 60 * time.Second, Params: map[string]string{"a": "1"}, Vantages: []string{"nyc"}},
		{Name: "a", ProbeKind: "FPing", Host: "h", Pings: 3, Step: 60 * time.Second, Params: map[string]string{"a": "1"}, Vantages: []string{"local"}},
	}
	if ConfigVersion(a) != ConfigVersion(b) {
		t.Errorf("ConfigVersion not order-independent:\n a=%s\n b=%s", ConfigVersion(a), ConfigVersion(b))
	}
	// A changed field changes the version.
	c := []model.Monitor{mon("a", "local"), mon("b", "nyc")}
	c[0].Host = "h2"
	if ConfigVersion(a) == ConfigVersion(c) {
		t.Error("ConfigVersion unchanged after Host changed")
	}
	// Format sanity.
	if v := ConfigVersion(a); len(v) < 8 || v[:7] != "sha256:" {
		t.Errorf("ConfigVersion format = %q, want sha256: prefix", v)
	}
}
