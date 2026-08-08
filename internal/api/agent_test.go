package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"smokeping-modern/internal/model"
	"smokeping-modern/internal/scheduler"
	"smokeping-modern/internal/store"
)

// recentTS returns an RFC3339Nano timestamp inside the ingest skew window, so ingest
// accept-path tests stay valid as wall-clock advances (P1-4 rejects stale timestamps).
func recentTS() string { return time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano) }

func testAgentServer() *Server {
	asg := map[string][]model.Monitor{
		"nyc": {{Name: "cf", ProbeKind: "FPing", Host: "1.1.1.1", Pings: 20, Step: time.Minute, Vantages: []string{"nyc"}}},
	}
	probeCfgs := map[string]map[string]string{"FPing": {"binary": "/usr/sbin/fping"}}
	return &Server{
		VantageAuth: fakeAuth{name: "nyc", ok: true},
		Assignment: func(v string) ([]model.Monitor, map[string]map[string]string, string) {
			return asg[v], probeCfgs, "sha256:v1-" + v
		},
	}
}

func TestAssignmentServed(t *testing.T) {
	srv := testAgentServer()
	r := httptest.NewRequest("GET", "/agent/v1/assignment", nil)
	r.Header.Set("Authorization", "Bearer smk_x_y")
	w := httptest.NewRecorder()
	srv.Routes().ServeHTTP(w, r)

	if w.Code != 200 {
		t.Fatalf("status=%d", w.Code)
	}
	if et := w.Header().Get("ETag"); et != "sha256:v1-nyc" {
		t.Fatalf("etag=%q", et)
	}
	var got struct {
		Vantage       string `json:"vantage"`
		ConfigVersion string `json:"config_version"`
		Targets       []struct {
			Name, Probe, Host string
			ProbeConfig       map[string]string `json:"probe_config"`
			StepMs            int64             `json:"step_ms"`
			Pings             int
		} `json:"targets"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Vantage != "nyc" || len(got.Targets) != 1 || got.Targets[0].StepMs != 60000 || got.Targets[0].Host != "1.1.1.1" {
		t.Fatalf("unexpected assignment: %+v", got)
	}
	// The hub's effective probe-level config for the target's kind rides along, so the
	// agent builds the probe the way the hub does (CODE_REVIEW #2 / P1-2).
	if got.Targets[0].ProbeConfig["binary"] != "/usr/sbin/fping" {
		t.Fatalf("probe_config not served: %+v", got.Targets[0].ProbeConfig)
	}
}

func TestAssignmentNotModified(t *testing.T) {
	srv := testAgentServer()
	r := httptest.NewRequest("GET", "/agent/v1/assignment", nil)
	r.Header.Set("Authorization", "Bearer smk_x_y")
	r.Header.Set("If-None-Match", "sha256:v1-nyc")
	w := httptest.NewRecorder()
	srv.Routes().ServeHTTP(w, r)
	if w.Code != http.StatusNotModified {
		t.Fatalf("status=%d want 304", w.Code)
	}
}

type fakeIngester struct {
	got []scheduler.Outcome
	err error
}

func (f *fakeIngester) Add(o []scheduler.Outcome)                   { f.got = append(f.got, o...) }
func (f *fakeIngester) Keys() ([]string, error)                     { return nil, nil }
func (f *fakeIngester) Latest(string) (scheduler.Outcome, bool)     { return scheduler.Outcome{}, false }
func (f *fakeIngester) History(string) ([]scheduler.Outcome, error) { return nil, nil }
func (f *fakeIngester) AddResults(_ context.Context, o []scheduler.Outcome) error {
	if f.err != nil {
		return f.err
	}
	f.got = append(f.got, o...)
	return nil
}

func ingestServer(ing store.Store) *Server {
	return &Server{
		store:       ing,
		VantageAuth: fakeAuth{name: "nyc", ok: true},
		Assignment: func(v string) ([]model.Monitor, map[string]map[string]string, string) {
			return []model.Monitor{{Name: "cf", ProbeKind: "FPing", Host: "1.1.1.1", Pings: 20, Step: time.Minute, Vantages: []string{"nyc"}}}, nil, "sha256:v1"
		},
	}
}

func postResults(t *testing.T, srv *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("POST", "/agent/v1/results", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer smk_x_y")
	w := httptest.NewRecorder()
	srv.Routes().ServeHTTP(w, r)
	return w
}

func TestIngestAcceptsAssignedTarget(t *testing.T) {
	ing := &fakeIngester{}
	srv := ingestServer(ing)
	w := postResults(t, srv,
		fmt.Sprintf(`{"results":[{"target":"cf","ts":%q,"pings":3,"rtts":[0.01,0.02,0.03]}]}`, recentTS()))
	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body)
	}
	if len(ing.got) != 1 {
		t.Fatalf("stored %d outcomes, want 1", len(ing.got))
	}
	o := ing.got[0]
	if o.Vantage != "nyc" || o.Target.Host != "1.1.1.1" || o.ProbeName != "FPing" {
		t.Fatalf("hub must fill canonical vantage/host/probe: %+v", o)
	}
	if o.Computed.Loss != 0 || o.Computed.Pings != 3 {
		t.Fatalf("Compute mismatch: %+v", o.Computed)
	}
}

func TestIngestDropsUnassignedTarget(t *testing.T) {
	ing := &fakeIngester{}
	srv := ingestServer(ing)
	w := postResults(t, srv,
		`{"results":[{"target":"not-mine","ts":"2026-08-07T12:00:00Z","pings":1,"rtts":[0.01]}]}`)
	if w.Code != 200 {
		t.Fatalf("status=%d", w.Code)
	}
	if len(ing.got) != 0 {
		t.Fatalf("unassigned target must be dropped, stored %d", len(ing.got))
	}
	var resp struct{ Accepted, Dropped int }
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Accepted != 0 || resp.Dropped != 1 {
		t.Fatalf("counts=%+v", resp)
	}
}

func TestIngestWriteErrorIs503(t *testing.T) {
	srv := ingestServer(&fakeIngester{err: errors.New("db down")})
	w := postResults(t, srv,
		fmt.Sprintf(`{"results":[{"target":"cf","ts":%q,"pings":1,"rtts":[0.01]}]}`, recentTS()))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d want 503", w.Code)
	}
}

// After a successful durable write, the hub must evaluate alerts for the accepted
// remote rounds (via OnIngest), passing each outcome's vantage (P2-5).
func TestIngestEvaluatesAlertsAfterStore(t *testing.T) {
	ing := &fakeIngester{}
	srv := ingestServer(ing)
	var seen []scheduler.Outcome
	srv.OnIngest = func(out []scheduler.Outcome) { seen = append(seen, out...) }
	w := postResults(t, srv,
		fmt.Sprintf(`{"results":[{"target":"cf","ts":%q,"pings":1,"rtts":[0.01]}]}`, recentTS()))
	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body)
	}
	if len(seen) != 1 || seen[0].Vantage != "nyc" || seen[0].Target.Name != "cf" {
		t.Fatalf("OnIngest must receive stored remote outcomes with their vantage, got %+v", seen)
	}
}

// A failed durable write must return 503 and NOT evaluate alerts — a firing must be
// backed by persisted data, and the agent will retry the batch (P2-5).
func TestIngestSkipsAlertsOnStoreError(t *testing.T) {
	srv := ingestServer(&fakeIngester{err: errors.New("db down")})
	called := false
	srv.OnIngest = func([]scheduler.Outcome) { called = true }
	w := postResults(t, srv,
		fmt.Sprintf(`{"results":[{"target":"cf","ts":%q,"pings":1,"rtts":[0.01]}]}`, recentTS()))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d want 503", w.Code)
	}
	if called {
		t.Fatal("OnIngest must not run when the durable write failed")
	}
}

// TestIngestDropsMalformedRounds covers the per-round validation in agentResults:
// each case is dropped (never reaches the store) without failing the whole batch.
func TestIngestDropsMalformedRounds(t *testing.T) {
	ts := recentTS()
	future := time.Now().UTC().Add(10 * time.Minute).Format(time.RFC3339Nano)
	stale := time.Now().UTC().Add(-40 * 24 * time.Hour).Format(time.RFC3339Nano)
	cases := []struct {
		name string
		body string
	}{
		{"unparseable ts", `{"results":[{"target":"cf","ts":"not-a-time","pings":3,"rtts":[0.01]}]}`},
		{"pings below 1", fmt.Sprintf(`{"results":[{"target":"cf","ts":%q,"pings":0,"rtts":[]}]}`, ts)},
		{"pings above MaxPings", fmt.Sprintf(`{"results":[{"target":"cf","ts":%q,"pings":10001,"rtts":[0.01]}]}`, ts)},
		{"more rtts than pings", fmt.Sprintf(`{"results":[{"target":"cf","ts":%q,"pings":1,"rtts":[0.01,0.02]}]}`, ts)},
		// P1-4: numeric + timestamp sanity (all JSON-reachable garbage that would poison the series).
		{"negative rtt", fmt.Sprintf(`{"results":[{"target":"cf","ts":%q,"pings":1,"rtts":[-0.5]}]}`, ts)},
		{"absurdly large rtt", fmt.Sprintf(`{"results":[{"target":"cf","ts":%q,"pings":1,"rtts":[1e30]}]}`, ts)},
		{"negative duration", fmt.Sprintf(`{"results":[{"target":"cf","ts":%q,"pings":1,"rtts":[0.01],"duration_ms":-5}]}`, ts)},
		{"absurd duration", fmt.Sprintf(`{"results":[{"target":"cf","ts":%q,"pings":1,"rtts":[0.01],"duration_ms":1e30}]}`, ts)},
		{"far-future ts", fmt.Sprintf(`{"results":[{"target":"cf","ts":%q,"pings":1,"rtts":[0.01]}]}`, future)},
		{"stale ts beyond horizon", fmt.Sprintf(`{"results":[{"target":"cf","ts":%q,"pings":1,"rtts":[0.01]}]}`, stale)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ing := &fakeIngester{}
			srv := ingestServer(ing)
			w := postResults(t, srv, tc.body)
			if w.Code != 200 {
				t.Fatalf("status=%d body=%s", w.Code, w.Body)
			}
			if len(ing.got) != 0 {
				t.Fatalf("malformed round must not be written, stored %d", len(ing.got))
			}
			var resp struct{ Accepted, Dropped int }
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if resp.Accepted != 0 || resp.Dropped != 1 {
				t.Fatalf("counts=%+v, want accepted=0 dropped=1", resp)
			}
		})
	}
}

// TestValidSamples covers the numeric guard directly, including the NaN/Inf cases that
// cannot be expressed in a JSON request body (P1-4).
func TestValidSamples(t *testing.T) {
	inf := math.Inf(1)
	nan := math.NaN()
	cases := []struct {
		name     string
		rtts     []float64
		duration float64
		want     bool
	}{
		{"clean", []float64{0.01, 0.02}, 15, true},
		{"zero rtt ok", []float64{0}, 0, true},
		{"NaN rtt", []float64{nan}, 0, false},
		{"+Inf rtt", []float64{inf}, 0, false},
		{"-Inf rtt", []float64{-inf}, 0, false},
		{"negative rtt", []float64{-0.001}, 0, false},
		{"too-large rtt", []float64{maxRTTSeconds + 1}, 0, false},
		{"NaN duration", []float64{0.01}, nan, false},
		{"Inf duration", []float64{0.01}, inf, false},
		{"negative duration", []float64{0.01}, -1, false},
		{"too-large duration", []float64{0.01}, maxDurationMs + 1, false},
	}
	for _, tc := range cases {
		if got := validSamples(tc.rtts, tc.duration); got != tc.want {
			t.Errorf("%s: validSamples=%v want %v", tc.name, got, tc.want)
		}
	}
}

func TestWithinSkew(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	cases := []struct {
		name string
		ts   time.Time
		want bool
	}{
		{"now", now, true},
		{"recent past", now.Add(-time.Hour), true},
		{"small future skew", now.Add(time.Minute), true},
		{"far future", now.Add(maxFutureSkew + time.Minute), false},
		{"at past horizon", now.Add(-maxPastSkew + time.Hour), true},
		{"beyond past horizon", now.Add(-maxPastSkew - time.Hour), false},
	}
	for _, tc := range cases {
		if got := withinSkew(tc.ts, now); got != tc.want {
			t.Errorf("%s: withinSkew=%v want %v", tc.name, got, tc.want)
		}
	}
}

// TestReservedVantageForbidden covers the defense-in-depth check (belt-and-suspenders
// alongside the mint-time block in vantage.Store.Add): an agent that somehow
// authenticates as the hub's own "local" vantage must be refused before any
// assignment lookup or store work, so its rounds can never conflate with the hub's
// own authoritative data.
func TestReservedVantageForbidden(t *testing.T) {
	reserved := fakeAuth{name: "local", ok: true}

	t.Run("assignment", func(t *testing.T) {
		srv := testAgentServer()
		srv.VantageAuth = reserved
		r := httptest.NewRequest("GET", "/agent/v1/assignment", nil)
		r.Header.Set("Authorization", "Bearer smk_x_y")
		w := httptest.NewRecorder()
		srv.Routes().ServeHTTP(w, r)
		if w.Code != http.StatusForbidden {
			t.Fatalf("status=%d want 403", w.Code)
		}
	})

	t.Run("results", func(t *testing.T) {
		ing := &fakeIngester{}
		srv := ingestServer(ing)
		srv.VantageAuth = reserved
		w := postResults(t, srv,
			`{"results":[{"target":"cf","ts":"2026-08-07T12:00:00Z","pings":1,"rtts":[0.01]}]}`)
		if w.Code != http.StatusForbidden {
			t.Fatalf("status=%d want 403", w.Code)
		}
		if len(ing.got) != 0 {
			t.Fatalf("reserved-vantage request must not reach the store, stored %d", len(ing.got))
		}
	})
}
