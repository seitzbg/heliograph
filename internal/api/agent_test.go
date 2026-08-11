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

	"smokeping-modern/internal/federation"
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
	got  []scheduler.Outcome
	seen map[string]bool // (vantage,target,ts) already stored — models the DB's uniqueness
	err  error
}

func (f *fakeIngester) Add(o []scheduler.Outcome)                   { f.got = append(f.got, o...) }
func (f *fakeIngester) Keys() ([]string, error)                     { return nil, nil }
func (f *fakeIngester) Latest(string) (scheduler.Outcome, bool)     { return scheduler.Outcome{}, false }
func (f *fakeIngester) History(string) ([]scheduler.Outcome, error) { return nil, nil }

// AddResults models the real stores: it stores each (vantage,target,ts) once and returns
// only the newly-inserted subset, so a replayed round is not re-evaluated (CODE_REVIEW #4).
func (f *fakeIngester) AddResults(_ context.Context, o []scheduler.Outcome) ([]scheduler.Outcome, error) {
	if f.err != nil {
		return nil, f.err
	}
	if f.seen == nil {
		f.seen = map[string]bool{}
	}
	var inserted []scheduler.Outcome
	for _, x := range o {
		k := x.Vantage + "\x00" + x.Target.Name + "\x00" + x.When.UTC().Format(time.RFC3339Nano)
		if f.seen[k] {
			continue
		}
		f.seen[k] = true
		f.got = append(f.got, x)
		inserted = append(inserted, x)
	}
	return inserted, nil
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

// Audit M1: a round whose self-reported `pings` exceeds the assigned monitor's
// pings must be dropped, not accepted. ingestServer pins cf.Pings=20; a round
// claiming pings=10000 with no RTTs would otherwise make sample.Compute allocate
// a 10000-element array per round from a tiny request body — a memory
// amplification vector reachable by any authenticated vantage. Bounding the
// per-round pings by the authoritative assignment closes it.
func TestIngestDropsRoundExceedingAssignedPings(t *testing.T) {
	ing := &fakeIngester{}
	srv := ingestServer(ing)
	w := postResults(t, srv,
		fmt.Sprintf(`{"results":[{"target":"cf","ts":%q,"pings":10000,"rtts":[]}]}`, recentTS()))
	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body)
	}
	var resp struct{ Accepted, Dropped int }
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Accepted != 0 || resp.Dropped != 1 {
		t.Fatalf("round exceeding assigned pings must be dropped: accepted=%d dropped=%d", resp.Accepted, resp.Dropped)
	}
	if len(ing.got) != 0 {
		t.Fatalf("a round over the assigned ping count must not be stored, got %d", len(ing.got))
	}
}

// The assignment must stamp each target with federation.Fingerprint over its current
// identity, so the agent can echo it back and the hub can verify attribution on ingest.
func TestAssignmentStampsFingerprint(t *testing.T) {
	srv := testAgentServer()
	r := httptest.NewRequest("GET", "/agent/v1/assignment", nil)
	r.Header.Set("Authorization", "Bearer smk_x_y")
	w := httptest.NewRecorder()
	srv.Routes().ServeHTTP(w, r)

	var got struct {
		Targets []struct {
			Fingerprint string `json:"fingerprint"`
		} `json:"targets"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// testAgentServer's monitor: cf / FPing / 1.1.1.1 / pings 20, probe cfg binary=/usr/sbin/fping.
	want := federation.Fingerprint(
		model.Monitor{Name: "cf", ProbeKind: "FPing", Host: "1.1.1.1", Pings: 20, Step: time.Minute, Vantages: []string{"nyc"}},
		map[string]string{"binary": "/usr/sbin/fping"},
	)
	if len(got.Targets) != 1 || got.Targets[0].Fingerprint != want {
		t.Fatalf("assignment fingerprint = %q, want %q", got.Targets[0].Fingerprint, want)
	}
}

// The core of CODE_REVIEW #2: a buffered round measured under a target's OLD identity must
// not be stored (or alerted) as the target's redefined identity. A round carrying the
// CURRENT fingerprint is accepted; a stale one is dropped and counted, not misattributed.
func TestIngestFingerprintAttribution(t *testing.T) {
	current := model.Monitor{Name: "cf", ProbeKind: "FPing", Host: "2.2.2.2", Pings: 3, Step: time.Minute, Vantages: []string{"nyc"}}
	probeCfgs := map[string]map[string]string{"FPing": {"binary": "/usr/sbin/fping"}}
	ing := &fakeIngester{}
	var alerted []scheduler.Outcome
	srv := &Server{
		store:       ing,
		VantageAuth: fakeAuth{name: "nyc", ok: true},
		Assignment: func(v string) ([]model.Monitor, map[string]map[string]string, string) {
			return []model.Monitor{current}, probeCfgs, "sha256:v1"
		},
		OnIngest: func(o []scheduler.Outcome) { alerted = append(alerted, o...) },
	}
	fpCurrent := federation.Fingerprint(current, probeCfgs["FPing"])
	old := current
	old.Host = "1.1.1.1" // the identity the agent actually measured under, since redefined
	fpOld := federation.Fingerprint(old, probeCfgs["FPing"])

	// Stale-identity round: dropped, not stored, not alerted.
	w := postResults(t, srv, fmt.Sprintf(
		`{"results":[{"target":"cf","ts":%q,"pings":3,"rtts":[0.01,0.02,0.03],"fingerprint":%q}]}`, recentTS(), fpOld))
	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body)
	}
	var resp struct{ Accepted, Dropped int }
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Accepted != 0 || resp.Dropped != 1 {
		t.Fatalf("stale-fingerprint round: counts=%+v, want accepted=0 dropped=1", resp)
	}
	if len(ing.got) != 0 {
		t.Fatalf("stale-fingerprint round must not be stored, stored %d", len(ing.got))
	}
	if len(alerted) != 0 {
		t.Fatalf("stale-fingerprint round must not be alerted, alerted %d", len(alerted))
	}

	// Current-identity round: accepted and stored under the current host.
	w = postResults(t, srv, fmt.Sprintf(
		`{"results":[{"target":"cf","ts":%q,"pings":3,"rtts":[0.01,0.02,0.03],"fingerprint":%q}]}`, recentTS(), fpCurrent))
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Accepted != 1 || resp.Dropped != 0 {
		t.Fatalf("current-fingerprint round: counts=%+v, want accepted=1 dropped=0", resp)
	}
	if len(ing.got) != 1 || ing.got[0].Target.Host != "2.2.2.2" {
		t.Fatalf("current-fingerprint round must be stored as current host: %+v", ing.got)
	}
	// The validated fingerprint rides on the stored outcome so a later reload can drop a
	// remote round that crossed a redefinition, the same way it drops a local one (#4).
	if ing.got[0].Fingerprint != fpCurrent {
		t.Fatalf("ingested outcome must carry its fingerprint, got %q want %q", ing.got[0].Fingerprint, fpCurrent)
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

// A replayed round (an HTTP retry, or the agent's deliberate resend of an already-delivered
// split half) is deduplicated by the store, so the hub must store AND evaluate it exactly
// once — otherwise one lost round replayed twice could satisfy an X=2 consecutive-loss
// matcher and emit a false FIRING (CODE_REVIEW #4/replay).
func TestIngestReplayStoredAndEvaluatedOnce(t *testing.T) {
	ing := &fakeIngester{}
	srv := ingestServer(ing)
	var seen []scheduler.Outcome
	srv.OnIngest = func(out []scheduler.Outcome) { seen = append(seen, out...) }
	body := fmt.Sprintf(`{"results":[{"target":"cf","ts":%q,"pings":1,"rtts":[0.01]}]}`, recentTS())
	for i := 0; i < 2; i++ { // submit the identical round twice
		if w := postResults(t, srv, body); w.Code != 200 {
			t.Fatalf("post %d: status=%d body=%s", i, w.Code, w.Body)
		}
	}
	if len(ing.got) != 1 {
		t.Fatalf("replayed round must be stored once, got %d", len(ing.got))
	}
	if len(seen) != 1 {
		t.Fatalf("replayed round must be alert-evaluated once, got %d", len(seen))
	}
}

// An empty-fingerprint round (a pre-fingerprint agent) is accepted transitionally, but must be
// counted per vantage on /metrics so an operator can watch a rolling agent upgrade complete —
// the counter stops rising — rather than relying on a single process-wide log line (#2).
func TestIngestMissingFingerprintMetric(t *testing.T) {
	srv := ingestServer(&fakeIngester{})
	body := fmt.Sprintf(`{"results":[{"target":"cf","ts":%q,"pings":1,"rtts":[0.01]}]}`, recentTS())
	if w := postResults(t, srv, body); w.Code != 200 { // no "fingerprint" field
		t.Fatalf("status=%d body=%s", w.Code, w.Body)
	}
	r := httptest.NewRequest("GET", "/metrics", nil)
	w := httptest.NewRecorder()
	srv.Routes().ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("/metrics status=%d", w.Code)
	}
	want := `smokeping_agent_missing_fingerprint_total{vantage="nyc"} 1`
	if !strings.Contains(w.Body.String(), want) {
		t.Fatalf("/metrics should expose %q, got:\n%s", want, w.Body.String())
	}
}

// Strict mode (RequireFingerprint): a round with no fingerprint is a visible permanent drop —
// not stored, not alerted — so an unverifiable round can't be misattributed across a target
// redefinition. Lenient mode (default) still accepts it. Both count it on /metrics (CODE_REVIEW #2).
func TestIngestStrictFingerprintDropsEmpty(t *testing.T) {
	body := func() string {
		return fmt.Sprintf(`{"results":[{"target":"cf","ts":%q,"pings":1,"rtts":[0.01]}]}`, recentTS())
	}
	// Strict: dropped, not stored, not alerted.
	ing := &fakeIngester{}
	srv := ingestServer(ing)
	srv.RequireFingerprint = true
	var alerted int
	srv.OnIngest = func(o []scheduler.Outcome) { alerted += len(o) }
	w := postResults(t, srv, body())
	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body)
	}
	var resp struct{ Accepted, Dropped int }
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Accepted != 0 || resp.Dropped != 1 {
		t.Fatalf("strict mode: counts=%+v, want accepted=0 dropped=1", resp)
	}
	if len(ing.got) != 0 || alerted != 0 {
		t.Fatalf("strict mode must not store or alert an empty-fingerprint round: stored=%d alerted=%d", len(ing.got), alerted)
	}
	// Still counted on /metrics so the operator sees it.
	mr := httptest.NewRequest("GET", "/metrics", nil)
	mw := httptest.NewRecorder()
	srv.Routes().ServeHTTP(mw, mr)
	if !strings.Contains(mw.Body.String(), `smokeping_agent_missing_fingerprint_total{vantage="nyc"} 1`) {
		t.Fatalf("strict drop should still be counted on /metrics, got:\n%s", mw.Body.String())
	}
	// The HELP text must not claim these rounds were "accepted": in strict mode they were
	// DROPPED. The counter is rounds RECEIVED without a fingerprint, whether accepted (lenient)
	// or dropped (strict), so dashboards/alerts don't misstate rejected traffic (CODE_REVIEW #4).
	help := mw.Body.String()
	if !strings.Contains(help, "# HELP smokeping_agent_missing_fingerprint_total") ||
		!strings.Contains(help, "received with no measurement fingerprint") {
		t.Fatalf("HELP text should describe rounds RECEIVED (not accepted) without a fingerprint, got:\n%s", help)
	}
	if strings.Contains(help, "accepted with no measurement fingerprint") {
		t.Fatalf("HELP text still claims strict-mode drops were 'accepted':\n%s", help)
	}

	// Lenient (default): the same round is accepted and stored.
	ing2 := &fakeIngester{}
	srv2 := ingestServer(ing2) // RequireFingerprint defaults false
	w2 := postResults(t, srv2, body())
	_ = json.Unmarshal(w2.Body.Bytes(), &resp)
	if resp.Accepted != 1 || resp.Dropped != 0 || len(ing2.got) != 1 {
		t.Fatalf("lenient mode should accept the empty-fingerprint round: counts=%+v stored=%d", resp, len(ing2.got))
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
