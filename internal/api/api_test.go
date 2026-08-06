package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"smokeping-modern/internal/probe"
	"smokeping-modern/internal/sample"
	"smokeping-modern/internal/scheduler"
	"smokeping-modern/internal/store"
)

// rollupInternalErr stands in for a raw database error carrying internal detail
// (table names, driver internals) that must not reach an API caller.
const rollupInternalErr = `pq: relation "samples_hourly" does not exist`

// errRollupStore is a Rollupper whose Rollup always fails with an internal error.
type errRollupStore struct{ *store.MemStore }

func (errRollupStore) Rollup(context.Context, string, string, time.Time) ([]store.RollupPoint, error) {
	return nil, errors.New(rollupInternalErr)
}

func TestRollupHidesInternalError(t *testing.T) {
	srv := New(errRollupStore{store.NewMem(10)}, "")
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, httptest.NewRequest("GET", "/api/rollup?target=x", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, rollupInternalErr) {
		t.Errorf("response leaked the internal DB error to the client:\n%s", body)
	}
	if !strings.Contains(body, "rollup unavailable") {
		t.Errorf("expected a generic error message, got:\n%s", body)
	}
}

// unavailableRollupStore is a Rollupper whose aggregate was never created.
type unavailableRollupStore struct{ *store.MemStore }

func (unavailableRollupStore) Rollup(context.Context, string, string, time.Time) ([]store.RollupPoint, error) {
	return nil, store.ErrRollupUnavailable
}

// resRollupStore records the resolution and since passed to Rollup, so a test can
// assert /api/rollup routes ?res to the right tier and bounds ?window server-side.
type resRollupStore struct {
	*store.MemStore
	gotRes   string
	gotSince time.Time
}

func (rs *resRollupStore) Rollup(_ context.Context, _ string, resolution string, since time.Time) ([]store.RollupPoint, error) {
	rs.gotRes = resolution
	rs.gotSince = since
	return []store.RollupPoint{{Bucket: time.Unix(1_700_000_000, 0), MedianAvg: 0.01, MedianMin: 0.01, MedianMax: 0.02, Rounds: 3}}, nil
}

// /api/rollup?res selects the tier: default 1h, explicit 1d, anything else -> 400.
// The chosen resolution is echoed in the response and passed to the store.
func TestRollupResolution(t *testing.T) {
	rs := &resRollupStore{MemStore: store.NewMem(10)}
	srv := New(rs, "")
	do := func(q string) (int, string) {
		rec := httptest.NewRecorder()
		srv.Routes().ServeHTTP(rec, httptest.NewRequest("GET", "/api/rollup"+q, nil))
		var m struct {
			Resolution string `json:"resolution"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &m)
		return rec.Code, m.Resolution
	}

	if code, res := do("?target=x"); code != 200 || res != "1h" || rs.gotRes != "1h" {
		t.Errorf("default: code=%d res=%q gotRes=%q, want 200/1h/1h", code, res, rs.gotRes)
	}
	if code, res := do("?target=x&res=1d"); code != 200 || res != "1d" || rs.gotRes != "1d" {
		t.Errorf("res=1d: code=%d res=%q gotRes=%q, want 200/1d/1d", code, res, rs.gotRes)
	}
	if code, _ := do("?target=x&res=bogus"); code != http.StatusBadRequest {
		t.Errorf("res=bogus: code=%d, want 400", code)
	}
}

// /api/rollup?window bounds the buckets server-side: it passes a cutoff ~window ago
// to the store; no window means the full history (zero since); a bad window is 400.
func TestRollupWindow(t *testing.T) {
	rs := &resRollupStore{MemStore: store.NewMem(10)}
	srv := New(rs, "")
	get := func(q string) int {
		rec := httptest.NewRecorder()
		srv.Routes().ServeHTTP(rec, httptest.NewRequest("GET", "/api/rollup"+q, nil))
		return rec.Code
	}

	// window=240h (10 days) -> since ~10 days ago.
	rs.gotSince = time.Time{}
	if code := get("?target=x&res=1h&window=240h"); code != 200 {
		t.Fatalf("window=240h status = %d", code)
	}
	if d := time.Since(rs.gotSince); d < 239*time.Hour || d > 241*time.Hour {
		t.Errorf("since = %v ago, want ~240h", d)
	}

	// no window -> zero since (full history).
	rs.gotSince = time.Unix(1, 0)
	if code := get("?target=x&res=1h"); code != 200 {
		t.Fatalf("no-window status = %d", code)
	}
	if !rs.gotSince.IsZero() {
		t.Errorf("no window should pass a zero since, got %v", rs.gotSince)
	}

	// bad window -> 400.
	if code := get("?target=x&window=zzz"); code != http.StatusBadRequest {
		t.Errorf("window=zzz status = %d, want 400", code)
	}
}

// A missing hourly aggregate (store implements Rollupper but the view isn't
// there) must answer 501 so the UI disables hourly mode, not 503.
func TestRollupMissingAggregateReturns501(t *testing.T) {
	srv := New(unavailableRollupStore{store.NewMem(10)}, "")
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, httptest.NewRequest("GET", "/api/rollup?target=x", nil))
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501 for a missing hourly aggregate", rec.Code)
	}
}

// Live endpoints report only currently-configured targets: one removed on reload
// (present in the store, absent from Active) must not show as healthy.
func TestLiveEndpointsFilterToActiveTargets(t *testing.T) {
	st := store.NewMem(10)
	st.Add([]scheduler.Outcome{
		{Target: probe.Target{Name: "kept", Host: "h"}, ProbeName: "FPing",
			Computed: sample.Compute(2, []float64{0.01, 0.02}), When: time.Unix(1_700_000_000, 0)},
		{Target: probe.Target{Name: "removed", Host: "h"}, ProbeName: "FPing",
			Computed: sample.Compute(2, []float64{0.01, 0.02}), When: time.Unix(1_700_000_000, 0)},
	})
	srv := New(st, "")
	srv.Active = func() map[string]bool { return map[string]bool{"kept": true} }

	// /api/targets excludes the removed target.
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, httptest.NewRequest("GET", "/api/targets", nil))
	var tj struct {
		Targets []struct {
			Name string `json:"name"`
		} `json:"targets"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &tj); err != nil {
		t.Fatalf("targets json: %v", err)
	}
	if len(tj.Targets) != 1 || tj.Targets[0].Name != "kept" {
		t.Errorf("/api/targets = %+v, want only \"kept\"", tj.Targets)
	}

	// /metrics excludes the removed target's smokeping_probe_up and exports a
	// last-sample timestamp for the kept one.
	rec = httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	body := rec.Body.String()
	if strings.Contains(body, `target="removed"`) {
		t.Errorf("/metrics still exports the removed target:\n%s", body)
	}
	if !strings.Contains(body, `smokeping_probe_last_sample_timestamp_seconds{target="kept",probe="FPing"} 1700000000`) {
		t.Errorf("/metrics missing per-target last-sample timestamp:\n%s", body)
	}
}

func TestMetricsEndpoint(t *testing.T) {
	st := store.NewMem(10)
	st.Add([]scheduler.Outcome{
		{Target: probe.Target{Name: "a b/c", Host: "h"}, ProbeName: "FPing",
			Computed: sample.Compute(4, []float64{0.01, 0.02, 0.03, 0.04})}, // no loss, median 0.03
		{Target: probe.Target{Name: "down", Host: "h"}, ProbeName: "TCPConnect",
			Computed: sample.Compute(4, nil)}, // total loss
	})
	srv := New(st, "")

	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()

	want := []string{
		`smokeping_probe_median_seconds{target="a b/c",probe="FPing"} 0.03`,
		`smokeping_probe_loss_ratio{target="a b/c",probe="FPing"} 0`,
		`smokeping_probe_up{target="a b/c",probe="FPing"} 1`,
		`smokeping_probe_median_seconds{target="down",probe="TCPConnect"} NaN`,
		`smokeping_probe_loss_ratio{target="down",probe="TCPConnect"} 1`,
		`smokeping_probe_up{target="down",probe="TCPConnect"} 0`,
		"# TYPE smokeping_probe_median_seconds gauge",
	}
	for _, s := range want {
		if !strings.Contains(body, s) {
			t.Errorf("metrics output missing line:\n  %s\n--- got ---\n%s", s, body)
		}
	}
	// Without a RoundStats, no round metrics are emitted.
	if strings.Contains(body, "smokeping_rounds_total") {
		t.Errorf("round metrics emitted without a RoundStats:\n%s", body)
	}
}

func TestChartsRanking(t *testing.T) {
	st := store.NewMem(10)
	st.Add([]scheduler.Outcome{
		// low latency, no loss
		{Target: probe.Target{Name: "fast", Host: "h"}, ProbeName: "FPing",
			Computed: sample.Compute(4, []float64{0.01, 0.011, 0.012, 0.013})},
		// high latency, no loss, high jitter
		{Target: probe.Target{Name: "slow", Host: "h"}, ProbeName: "FPing",
			Computed: sample.Compute(4, []float64{0.10, 0.20, 0.30, 0.40})},
		// fully lost -> 100% loss, no median/stddev
		{Target: probe.Target{Name: "down", Host: "h"}, ProbeName: "TCPConnect",
			Computed: sample.Compute(4, nil)},
	})
	srv := New(st, "")

	get := func(q string) map[string]any {
		rec := httptest.NewRecorder()
		srv.Routes().ServeHTTP(rec, httptest.NewRequest("GET", "/api/charts"+q, nil))
		if rec.Code != 200 {
			t.Fatalf("%s: status %d", q, rec.Code)
		}
		var m map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return m
	}
	names := func(m map[string]any) []string {
		var out []string
		for _, c := range m["charts"].([]any) {
			out = append(out, c.(map[string]any)["name"].(string))
		}
		return out
	}

	// by=loss (default): "down" (100%) first.
	if got := names(get("")); len(got) == 0 || got[0] != "down" {
		t.Errorf("by=loss: got %v, want down first", got)
	}
	// by=median: "down" excluded (no median); "slow" ranks above "fast".
	got := names(get("?by=median"))
	if contains(got, "down") {
		t.Errorf("by=median must exclude the fully-lost target, got %v", got)
	}
	if len(got) < 2 || got[0] != "slow" {
		t.Errorf("by=median: got %v, want slow first", got)
	}
	// by=stddev: "slow" (spread 0.1..0.4) beats "fast" (tight); "down" excluded.
	got = names(get("?by=stddev"))
	if len(got) == 0 || got[0] != "slow" || contains(got, "down") {
		t.Errorf("by=stddev: got %v, want slow first, no down", got)
	}
	// n caps the result.
	if got := names(get("?by=loss&n=1")); len(got) != 1 {
		t.Errorf("n=1: got %d entries, want 1", len(got))
	}
	// invalid by -> 400.
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, httptest.NewRequest("GET", "/api/charts?by=bogus", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("by=bogus: status %d, want 400", rec.Code)
	}
}

func contains(s []string, want string) bool {
	for _, v := range s {
		if v == want {
			return true
		}
	}
	return false
}

func TestSLAOf(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0)
	mk := func(off time.Duration, lossPct float64, pings int) scheduler.Outcome {
		lost := int(float64(pings) * lossPct / 100)
		var rtts []float64
		for i := 0; i < pings-lost; i++ {
			rtts = append(rtts, 0.01)
		}
		return scheduler.Outcome{When: t0.Add(off), Computed: sample.Compute(pings, rtts)}
	}
	hist := []scheduler.Outcome{
		mk(0, 0, 4),               // t0: up (excluded by cutoff below)
		mk(1*time.Minute, 100, 4), // down
		mk(2*time.Minute, 50, 4),  // up (got replies)
		mk(3*time.Minute, 0, 4),   // up
	}
	up := func(lossPct float64) bool { return lossPct < 100 }
	rounds, upN, sumLoss, oldest, latest := slaOf(hist, t0.Add(1*time.Minute), up)
	if rounds != 3 || upN != 2 {
		t.Fatalf("rounds=%d up=%d, want 3 and 2", rounds, upN)
	}
	if latest != t0.Add(3*time.Minute) {
		t.Errorf("latest=%v, want t0+3m", latest)
	}
	if oldest != t0.Add(1*time.Minute) {
		t.Errorf("oldest=%v, want t0+1m (earliest in-window round)", oldest)
	}
	if avg := sumLoss / float64(rounds); avg < 49 || avg > 51 { // (100+50+0)/3 = 50
		t.Errorf("avg loss = %g, want ~50", avg)
	}
}

func TestSLAEndpoint(t *testing.T) {
	st := store.NewMem(100)
	now := time.Now()
	up4 := func(lossPct float64, when time.Time) scheduler.Outcome {
		var rtts []float64
		keep := 4 - int(4*lossPct/100)
		for i := 0; i < keep; i++ {
			rtts = append(rtts, 0.01)
		}
		return scheduler.Outcome{Target: probe.Target{Name: "a", Host: "h"}, ProbeName: "FPing",
			When: when, Computed: sample.Compute(4, rtts)}
	}
	// Target "a": within 24h -> 2 up (0%, 50%) + 1 down (100%); plus one old up round.
	st.Add([]scheduler.Outcome{up4(0, now.Add(-1*time.Minute))})
	st.Add([]scheduler.Outcome{up4(50, now.Add(-2*time.Minute))})
	st.Add([]scheduler.Outcome{up4(100, now.Add(-3*time.Minute))})
	st.Add([]scheduler.Outcome{up4(0, now.Add(-48*time.Hour))}) // outside 24h window
	// Target "b": fully down in-window.
	st.Add([]scheduler.Outcome{{Target: probe.Target{Name: "b", Host: "h"}, ProbeName: "FPing",
		When: now.Add(-1 * time.Minute), Computed: sample.Compute(4, nil)}})
	srv := New(st, "")

	get := func(q string) []map[string]any {
		rec := httptest.NewRecorder()
		srv.Routes().ServeHTTP(rec, httptest.NewRequest("GET", "/api/sla"+q, nil))
		if rec.Code != 200 {
			t.Fatalf("%s: status %d", q, rec.Code)
		}
		var m map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		var out []map[string]any
		for _, e := range m["targets"].([]any) {
			out = append(out, e.(map[string]any))
		}
		return out
	}

	rows := get("?window=24h")
	byName := map[string]map[string]any{}
	for _, r := range rows {
		byName[r["name"].(string)] = r
	}
	// "a": 3 in-window rounds, 2 up -> 66.7%; old round excluded.
	if a := byName["a"]; a["measured"].(float64) != 3 || a["availability"].(float64) < 66 || a["availability"].(float64) > 67 {
		t.Errorf("a: measured=%v avail=%v, want 3 measured ~66.7%%", a["measured"], a["availability"])
	}
	// "b": fully down -> 0% availability, and sorted first (worst).
	if rows[0]["name"] != "b" || rows[0]["availability"].(float64) != 0 {
		t.Errorf("worst-first: got %v (avail %v), want b at 0%%", rows[0]["name"], rows[0]["availability"])
	}
	// maxloss=20: the 50%-loss round now counts as down -> a has 1/3 up.
	a := map[string]map[string]any{}
	for _, r := range get("?window=24h&maxloss=20") {
		a[r["name"].(string)] = r
	}
	if av := a["a"]["availability"].(float64); av < 33 || av > 34 {
		t.Errorf("maxloss=20: a availability = %v, want ~33.3%%", av)
	}
	// invalid window -> 400.
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, httptest.NewRequest("GET", "/api/sla?window=zzz", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("window=zzz: status %d, want 400", rec.Code)
	}
}

// /api/sla reports coverage: measured rounds vs the rounds expected over the
// window from each target's step. Availability stays up/measured (gaps = unknown).
func TestSLACoverage(t *testing.T) {
	st := store.NewMem(100)
	now := time.Now()
	add := func(name string, off time.Duration, rtts []float64) {
		st.Add([]scheduler.Outcome{{Target: probe.Target{Name: name, Host: "h"}, ProbeName: "FPing",
			When: now.Add(off), Computed: sample.Compute(4, rtts)}})
	}
	// "a": step 1h, window 24h -> expected 24. 6 measured rounds, 1 fully down.
	for i := 1; i <= 5; i++ {
		add("a", -time.Duration(i)*time.Minute, []float64{.01, .01, .01, .01})
	}
	add("a", -6*time.Minute, nil) // down
	// "b": has data but no known step -> coverage omitted.
	add("b", -1*time.Minute, []float64{.01, .01, .01, .01})

	srv := New(st, "")
	srv.Steps = func() map[string]time.Duration { return map[string]time.Duration{"a": time.Hour} }

	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, httptest.NewRequest("GET", "/api/sla?window=24h", nil))
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	var m struct {
		Targets []struct {
			Name         string   `json:"name"`
			Measured     int      `json:"measured"`
			UpRounds     int      `json:"up_rounds"`
			Availability float64  `json:"availability"`
			Expected     *int     `json:"expected"`
			CoveragePct  *float64 `json:"coverage_pct"`
		} `json:"targets"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("json: %v", err)
	}
	byName := map[string]struct {
		Name         string   `json:"name"`
		Measured     int      `json:"measured"`
		UpRounds     int      `json:"up_rounds"`
		Availability float64  `json:"availability"`
		Expected     *int     `json:"expected"`
		CoveragePct  *float64 `json:"coverage_pct"`
	}{}
	for _, e := range m.Targets {
		byName[e.Name] = e
	}

	a := byName["a"]
	if a.Measured != 6 {
		t.Errorf("a.measured = %d, want 6", a.Measured)
	}
	if a.Expected == nil || *a.Expected != 24 { // 24h / 1h
		t.Errorf("a.expected = %v, want 24", a.Expected)
	}
	if a.CoveragePct == nil || *a.CoveragePct < 24.9 || *a.CoveragePct > 25.1 { // 6/24
		t.Errorf("a.coverage_pct = %v, want ~25", a.CoveragePct)
	}
	if a.Availability < 83.2 || a.Availability > 83.4 { // 5/6
		t.Errorf("a.availability = %v, want ~83.33", a.Availability)
	}
	// "b" has no known step -> no coverage.
	if b := byName["b"]; b.Expected != nil || b.CoveragePct != nil {
		t.Errorf("b coverage should be omitted, got expected=%v coverage=%v", b.Expected, b.CoveragePct)
	}
}

func TestProbeSchemaEndpoint(t *testing.T) {
	srv := New(store.NewMem(1), "")
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, httptest.NewRequest("GET", "/api/probes/schema", nil))
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	// The real probes are registered by cmd/smoked's blank imports, not here, so
	// this test asserts the envelope + JSON Schema markers are emitted; the shape
	// of an individual probe's schema is covered by probe.TestJSONSchemaShape.
	for _, want := range []string{`"probes"`, "application/json"} {
		if want == "application/json" {
			if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, want) {
				t.Errorf("Content-Type = %q, want %s", ct, want)
			}
			continue
		}
		if !strings.Contains(body, want) {
			t.Errorf("schema response missing %s:\n%s", want, body)
		}
	}
}

func TestMetricsPerProbeDurationAndRoundStats(t *testing.T) {
	st := store.NewMem(10)
	st.Add([]scheduler.Outcome{
		{Target: probe.Target{Name: "a", Host: "h"}, ProbeName: "FPing",
			Computed: sample.Compute(2, []float64{0.01, 0.02}), Duration: 250 * time.Millisecond},
	})
	srv := New(st, "")
	srv.Rounds = &RoundStats{}
	srv.Rounds.Observe(1500*time.Millisecond, 12, 3, time.Unix(1_700_000_000, 0))

	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	body := rec.Body.String()

	want := []string{
		`smokeping_probe_duration_seconds{target="a",probe="FPing"} 0.25`,
		"smokeping_rounds_total 1",
		"smokeping_round_duration_seconds 1.5",
		"smokeping_round_targets 12",
		"smokeping_round_errors 3",
		"smokeping_last_round_timestamp_seconds 1700000000",
	}
	for _, s := range want {
		if !strings.Contains(body, s) {
			t.Errorf("metrics output missing line:\n  %s\n--- got ---\n%s", s, body)
		}
	}
}

// rangeStore is a RangeHistorier that records the cutoff it was asked for and
// returns a single fixed round, so a test can distinguish the windowed path
// (HistorySince) from the capped History path (via the embedded MemStore).
type rangeStore struct {
	*store.MemStore
	called    bool
	gotCutoff time.Time
}

func (rs *rangeStore) HistorySince(_ context.Context, target string, cutoff time.Time) ([]scheduler.Outcome, error) {
	rs.called = true
	rs.gotCutoff = cutoff
	return []scheduler.Outcome{
		{Target: probe.Target{Name: target}, When: time.Unix(1_700_000_000, 0), Computed: sample.Compute(2, []float64{0.01, 0.02})},
	}, nil
}

// /api/series?window= must read the full window via HistorySince (fixing the
// 1024-round truncation of the 30h view); without window it keeps the capped
// History path; a bad window is a 400.
func TestSeriesWindow(t *testing.T) {
	base := store.NewMem(1024)
	now := time.Now()
	base.Add([]scheduler.Outcome{
		{Target: probe.Target{Name: "t"}, When: now.Add(-2 * time.Minute), Computed: sample.Compute(2, []float64{0.01, 0.02})},
		{Target: probe.Target{Name: "t"}, When: now.Add(-1 * time.Minute), Computed: sample.Compute(2, []float64{0.01, 0.02})},
	})
	rs := &rangeStore{MemStore: base}
	srv := New(rs, "")

	rounds := func(rec *httptest.ResponseRecorder) int {
		var resp struct {
			Rounds []map[string]any `json:"rounds"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("json: %v", err)
		}
		return len(resp.Rounds)
	}

	// No window -> capped History (2 rounds from the embedded MemStore); HistorySince untouched.
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, httptest.NewRequest("GET", "/api/series?target=t", nil))
	if rec.Code != 200 {
		t.Fatalf("no-window status = %d", rec.Code)
	}
	if rs.called {
		t.Error("no-window request must use History, not HistorySince")
	}
	if n := rounds(rec); n != 2 {
		t.Errorf("no-window rounds = %d, want 2 (History)", n)
	}

	// With window -> HistorySince, cutoff ~ now-30h, and its single round.
	rs.called = false
	rec = httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, httptest.NewRequest("GET", "/api/series?target=t&window=30h", nil))
	if rec.Code != 200 {
		t.Fatalf("window status = %d", rec.Code)
	}
	if !rs.called {
		t.Fatal("window request did not use HistorySince")
	}
	if d := time.Since(rs.gotCutoff); d < 29*time.Hour || d > 31*time.Hour {
		t.Errorf("cutoff = %v ago, want ~30h", d)
	}
	if n := rounds(rec); n != 1 {
		t.Errorf("window rounds = %d, want 1 (HistorySince stub)", n)
	}

	// Invalid window -> 400.
	rec = httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, httptest.NewRequest("GET", "/api/series?target=t&window=zzz", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("window=zzz status = %d, want 400", rec.Code)
	}
}

// countingStore records which store methods the API calls, to prove the live
// endpoints use the bulk queries (LatestAll / AvailabilityAll) rather than one
// Latest / Availability per target (the #5 fan-out reduction).
type countingStore struct {
	*store.MemStore
	latest, latestAll, avail, availAll int
}

func (c *countingStore) Latest(k string) (scheduler.Outcome, bool) {
	c.latest++
	return c.MemStore.Latest(k)
}
func (c *countingStore) LatestAll() (map[string]scheduler.Outcome, error) {
	c.latestAll++
	return c.MemStore.LatestAll()
}
func (c *countingStore) Availability(ctx context.Context, t string, cut time.Time, m *float64) (store.AvailabilityStat, error) {
	c.avail++
	return c.MemStore.Availability(ctx, t, cut, m)
}
func (c *countingStore) AvailabilityAll(ctx context.Context, cut time.Time, m *float64) (map[string]store.AvailabilityStat, error) {
	c.availAll++
	return c.MemStore.AvailabilityAll(ctx, cut, m)
}

func TestEndpointsUseBulkQueries(t *testing.T) {
	cs := &countingStore{MemStore: store.NewMem(100)}
	now := time.Now()
	for _, name := range []string{"a", "b", "c"} {
		cs.Add([]scheduler.Outcome{{Target: probe.Target{Name: name, Host: "h"}, ProbeName: "FPing",
			When: now.Add(-time.Minute), Computed: sample.Compute(4, []float64{0.01, 0.02, 0.03, 0.04})}})
	}
	srv := New(cs, "")

	call := func(path string) {
		rec := httptest.NewRecorder()
		srv.Routes().ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != 200 {
			t.Fatalf("%s: status %d", path, rec.Code)
		}
	}

	// /api/targets: one bulk LatestAll, zero per-target Latest.
	cs.latest, cs.latestAll = 0, 0
	call("/api/targets")
	if cs.latestAll != 1 || cs.latest != 0 {
		t.Errorf("/api/targets used latestAll=%d latest=%d, want 1/0", cs.latestAll, cs.latest)
	}

	// /api/charts and /api/metrics: same — bulk, no per-target Latest.
	for _, p := range []string{"/api/charts?by=loss", "/metrics"} {
		cs.latest, cs.latestAll = 0, 0
		call(p)
		if cs.latestAll != 1 || cs.latest != 0 {
			t.Errorf("%s used latestAll=%d latest=%d, want 1/0", p, cs.latestAll, cs.latest)
		}
	}

	// /api/sla: one bulk AvailabilityAll, zero per-target Availability.
	cs.avail, cs.availAll, cs.latest = 0, 0, 0
	call("/api/sla?window=24h")
	if cs.availAll != 1 || cs.avail != 0 {
		t.Errorf("/api/sla used availAll=%d avail=%d, want 1/0", cs.availAll, cs.avail)
	}
	if cs.latest != 0 {
		t.Errorf("/api/sla made %d per-target Latest calls, want 0 (uses bulk latest for probe names)", cs.latest)
	}
}

// errLatestStore is a LatestAller whose bulk read fails — simulating a database
// outage. The live endpoints must answer 503, not a false-empty 200 (CODE_REVIEW #4).
type errLatestStore struct{ *store.MemStore }

func (errLatestStore) LatestAll() (map[string]scheduler.Outcome, error) {
	return nil, errors.New("db read failed")
}

func TestReadFailureReturns503(t *testing.T) {
	srv := New(errLatestStore{store.NewMem(10)}, "")
	for _, p := range []string{"/api/targets", "/api/charts?by=loss", "/api/sla?window=24h", "/metrics"} {
		rec := httptest.NewRecorder()
		srv.Routes().ServeHTTP(rec, httptest.NewRequest("GET", p, nil))
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s on a store read failure = %d, want 503", p, rec.Code)
		}
	}
}
