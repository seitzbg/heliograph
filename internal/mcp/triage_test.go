package mcp

import (
	"context"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/seitzbg/heliograph/internal/api"
	"github.com/seitzbg/heliograph/internal/model"
	"github.com/seitzbg/heliograph/internal/probe"
	"github.com/seitzbg/heliograph/internal/sample"
	"github.com/seitzbg/heliograph/internal/scheduler"
	"github.com/seitzbg/heliograph/internal/store"
	"github.com/seitzbg/heliograph/internal/vantage"
)

func f(v float64) *float64 { return &v }

func TestClassify(t *testing.T) {
	cases := []struct {
		name string
		tg   Target
		want string
	}{
		{"nodata", Target{NoData: true}, "no_data"},
		{"error-down", Target{Error: "timeout"}, "down"},
		{"loss-down", Target{RecentLossPct: f(100)}, "down"},
		{"degraded", Target{RecentLossPct: f(10)}, "degraded"},
		{"healthy", Target{RecentLossPct: f(0)}, "healthy"},
		{"fallback-losspct", Target{LossPct: 50}, "degraded"},
	}
	for _, tc := range cases {
		if got := classify(tc.tg); got != tc.want {
			t.Errorf("%s: classify=%q want %q", tc.name, got, tc.want)
		}
	}
}

func TestTriageSplitsGlobalVsVantageSpecific(t *testing.T) {
	byV := map[string][]Target{
		"a": {{ID: "g", Name: "G", RecentLossPct: f(100)}, {ID: "v", Name: "V", RecentLossPct: f(100)}},
		"b": {{ID: "g", Name: "G", RecentLossPct: f(100)}, {ID: "v", Name: "V", RecentLossPct: f(0)}},
	}
	probs := analyzeTriage(byV)
	got := map[string]string{}
	for _, p := range probs {
		got[p.Target] = p.Scope
	}
	if got["G"] != "global" || got["V"] != "vantage-specific" {
		t.Fatalf("scopes wrong: %v", got)
	}
}

// TestCountHealthy pins countHealthy against per-target aggregation across ALL
// vantages, not "first row wins" (which was map-order-dependent and therefore
// non-deterministic across runs). H is healthy at every vantage and must count;
// V is healthy at "b" but down at "a" (a vantage-specific problem) and must NOT
// count; N is no_data at every vantage and must NOT count either.
func TestCountHealthy(t *testing.T) {
	byV := map[string][]Target{
		"a": {
			{ID: "h", Name: "H", RecentLossPct: f(0)},
			{ID: "v", Name: "V", RecentLossPct: f(100)},
			{ID: "n", Name: "N", NoData: true},
		},
		"b": {
			{ID: "h", Name: "H", RecentLossPct: f(0)},
			{ID: "v", Name: "V", RecentLossPct: f(0)},
			{ID: "n", Name: "N", NoData: true},
		},
	}
	if got := countHealthy(byV); got != 1 {
		t.Fatalf("countHealthy = %d, want 1 (only H is healthy from every vantage; V is bad at %q; N is no_data-only)", got, "a")
	}
}

// registry is a fake remote-vantage registry backing the hub's GET /api/admin/vantages.
type registry struct{ infos []vantage.Info }

func (r registry) Register(context.Context, string) error                   { return nil }
func (r registry) List(context.Context) ([]vantage.Info, error)             { return r.infos, nil }
func (r registry) Revoke(context.Context, string) (bool, error)             { return false, nil }
func (r registry) IsActive(context.Context, string, *big.Int) (bool, error) { return true, nil }
func (r registry) IssueClientCert(context.Context, string) ([]byte, []byte, []byte, error) {
	return nil, nil, nil, nil
}

// round builds one stored measurement round: 10 pings, all answered at 10ms unless lost.
func round(id, vant string, lost bool) scheduler.Outcome {
	var rtts []float64
	if !lost {
		rtts = []float64{.01, .01, .01, .01, .01, .01, .01, .01, .01, .01}
	}
	return scheduler.Outcome{
		Target: probe.Target{ID: id, Name: id}, ProbeName: "Ping",
		Computed: sample.Compute(10, rtts), When: time.Now(), Vantage: vant,
	}
}

// memoryHub serves the real hub API (internal/api) over a memory store holding monitors' rounds.
// With reg set it also enables the admin vantage registry, as a federated hub (-dsn) does, and
// publishes each target's vantage set the way cmd/smoked wires TargetVantages.
func memoryHub(t *testing.T, monitors []model.Monitor, rounds []scheduler.Outcome, reg *registry) *Client {
	t.Helper()
	st := store.NewMem(8)
	if _, err := st.AddResults(context.Background(), rounds); err != nil {
		t.Fatal(err)
	}
	srv := api.New(st, "")
	srv.Configured = func() []model.Monitor { return monitors }
	if reg != nil {
		srv.Vantages = *reg
		srv.AdminPassword = "admin-secret"
		srv.AdminKey = []byte("test-key-test-key-test-key-test1")
		srv.TargetVantages = func() map[string][]string {
			m := map[string][]string{}
			for _, mon := range monitors {
				m[mon.ID] = mon.Vantages
			}
			return m
		}
	}
	ts := httptest.NewServer(srv.Routes())
	t.Cleanup(ts.Close)
	c, err := NewClient(Config{BaseURL: ts.URL})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

type triageResult struct {
	Problems []Problem `json:"problems"`
	Stale    []string  `json:"stale_vantages"`
	Healthy  int       `json:"healthy_targets"`
	Checked  []string  `json:"vantages_checked"`
}

// TestTriageCoversLocalVantageAlongsideRemotes is the regression test for triage reading only the
// remote-vantage registry: the hub's built-in "local" vantage is never in it, so with a remote
// registered, a target down from local but healthy from the remote was reported healthy, and an
// explicit vantage=local was rejected as unknown.
func TestTriageCoversLocalVantageAlongsideRemotes(t *testing.T) {
	both := []string{"local", "remote"}
	monitors := []model.Monitor{
		{ID: "a", Name: "Sites/a", ProbeKind: "Ping", Host: "a.example", Vantages: both},
		{ID: "r", Name: "Sites/r", ProbeKind: "Ping", Host: "r.example", Vantages: []string{"remote"}},
	}
	rounds := []scheduler.Outcome{round("a", "local", true), round("a", "remote", false), round("r", "remote", false)}
	seen := time.Now()
	c := memoryHub(t, monitors, rounds, &registry{infos: []vantage.Info{
		{Name: "remote", Created: seen, LastSeen: seen},
		{Name: "dead", Created: seen}, // registered, never reported
	}})
	cs := mcpSession(t, c)

	var all triageResult
	if res := callTool(t, cs, "heliograph_triage", nil, &all); res.IsError {
		t.Fatalf("triage: %s", resultText(res))
	}
	if len(all.Problems) != 1 {
		t.Fatalf("problems=%+v, want exactly Sites/a", all.Problems)
	}
	p := all.Problems[0]
	if p.Target != "Sites/a" || p.Status != "down" || p.Scope != "vantage-specific" || !slices.Equal(p.Vantages, []string{"local"}) {
		t.Errorf("problem=%+v, want Sites/a down/vantage-specific at [local]", p)
	}
	if all.Healthy != 1 {
		t.Errorf("healthy=%d, want 1 (Sites/r, measured only from remote, must not read as no-data from local)", all.Healthy)
	}
	if !slices.Equal(all.Checked, both) {
		t.Errorf("vantages_checked=%v, want %v", all.Checked, both)
	}
	if !slices.Equal(all.Stale, []string{"dead"}) {
		t.Errorf("stale_vantages=%v, want [dead]", all.Stale)
	}

	var local triageResult
	if res := callTool(t, cs, "heliograph_triage", map[string]any{"vantage": "local"}, &local); res.IsError {
		t.Fatalf("triage vantage=local rejected: %s", resultText(res))
	}
	if len(local.Problems) != 1 || local.Problems[0].Target != "Sites/a" {
		t.Errorf("vantage=local problems=%+v, want Sites/a", local.Problems)
	}
	if res := callTool(t, cs, "heliograph_triage", map[string]any{"vantage": "nowhere"}, nil); !res.IsError {
		t.Error("triage vantage=nowhere: want an unknown-vantage error")
	}
}

// TestTriageWorksOnHubWithoutVantageRegistry is the regression test for triage failing outright on
// a plain single-host hub (no -dsn, no admin password), where GET /api/admin/vantages does not
// exist: triage must still classify the local measurements, and heliograph_vantages must explain
// the missing registry instead of surfacing a bare "404 page not found".
func TestTriageWorksOnHubWithoutVantageRegistry(t *testing.T) {
	monitors := []model.Monitor{{ID: "a", Name: "Sites/a", ProbeKind: "Ping", Host: "a.example", Vantages: []string{"local"}}}
	c := memoryHub(t, monitors, []scheduler.Outcome{round("a", "", true)}, nil)
	cs := mcpSession(t, c)

	var out triageResult
	res := callTool(t, cs, "heliograph_triage", nil, &out)
	if res.IsError {
		t.Fatalf("triage on a hub without the vantage registry: %s", resultText(res))
	}
	if len(out.Problems) != 1 || out.Problems[0].Target != "Sites/a" || out.Problems[0].Status != "down" {
		t.Errorf("problems=%+v, want Sites/a down", out.Problems)
	}
	if res := callTool(t, cs, "heliograph_triage", map[string]any{"vantage": "local"}, nil); res.IsError {
		t.Errorf("triage vantage=local: %s", resultText(res))
	}

	var vs struct {
		RegistryAvailable bool `json:"registry_available"`
	}
	res = callTool(t, cs, "heliograph_vantages", nil, &vs)
	if res.IsError {
		t.Fatalf("heliograph_vantages: %s", resultText(res))
	}
	if vs.RegistryAvailable || !strings.Contains(resultText(res), "registry") || strings.Contains(resultText(res), "404 page not found") {
		t.Errorf("heliograph_vantages on a hub without the registry: available=%v text=%q", vs.RegistryAvailable, resultText(res))
	}
}

// TestTriageSurfacesRegistryServerErrors pins that only an absent registry (404) is tolerated: a
// registry that exists but fails must fail triage rather than silently skip stale detection.
func TestTriageSurfacesRegistryServerErrors(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/targets":
			_, _ = io.WriteString(w, `{"targets":[{"id":"a","name":"A","loss_pct":0,"vantages":["local"]}]}`)
		case "/api/admin/vantages":
			http.Error(w, `{"error":"store unavailable"}`, http.StatusServiceUnavailable)
		default:
			http.NotFound(w, r)
		}
	}))
	res := callTool(t, mcpSession(t, c), "heliograph_triage", nil, nil)
	if !res.IsError || !strings.Contains(resultText(res), "503") {
		t.Fatalf("triage with a failing registry: error=%v text=%q, want the 503 surfaced", res.IsError, resultText(res))
	}
}
