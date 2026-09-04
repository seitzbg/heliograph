package mcp

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

// --- Fix 1: NewClient refuses cleartext credential transmission ---

func TestNewClientRejectsCleartextCredentials(t *testing.T) {
	cases := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{"http+basicuser", Config{BaseURL: "http://hub.example", BasicUser: "u"}, true},
		{"http+adminpass", Config{BaseURL: "http://hub.example", AdminPass: "s"}, true},
		{"http+nocreds", Config{BaseURL: "http://hub.example"}, false},
		{"https+creds", Config{BaseURL: "https://hub.example", BasicUser: "u", AdminPass: "s"}, false},
		{"http+loopback-ip+creds", Config{BaseURL: "http://127.0.0.1:8087", AdminPass: "s"}, false},
		{"http+localhost+creds", Config{BaseURL: "http://localhost:8087", BasicUser: "u"}, false},
	}
	for _, tc := range cases {
		_, err := NewClient(tc.cfg)
		if (err != nil) != tc.wantErr {
			t.Errorf("%s: NewClient err=%v, wantErr=%v", tc.name, err, tc.wantErr)
		}
	}
}

// --- Fix 2: fetchConfig rejects unsupported source/format before any network call ---

func TestFetchConfigRejectsBadSourceAndFormat(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("no request should be made for an invalid source/format; got %s", r.URL.Path)
	}))
	if _, _, err := fetchConfig(context.Background(), c, "db", "toml"); err == nil {
		t.Error("expected error for format=toml")
	}
	if _, _, err := fetchConfig(context.Background(), c, "bogus", "yaml"); err == nil {
		t.Error("expected error for source=bogus")
	}
}

// --- Fix 4: fetchSeries rejects a partial [from,to] range before any network call ---

func TestFetchSeriesRejectsPartialRange(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("no request should be made for a partial range; got %s", r.URL.RawQuery)
	}))
	if _, _, err := fetchSeries(context.Background(), c, "id", "", "", 100, 0, false, 500); err == nil {
		t.Error("expected error for lone from")
	}
	if _, _, err := fetchSeries(context.Background(), c, "id", "", "", 0, 200, false, 500); err == nil {
		t.Error("expected error for lone to")
	}
}

// --- Fix 6: a no-data vantage downgrades global to vantage-specific ---

func TestAnalyzeTriageNoDataDowngradesGlobal(t *testing.T) {
	byV := map[string][]Target{
		// X: down at a, NO-DATA at b  -> not confirmed bad everywhere -> vantage-specific
		// Y: down at a AND b          -> bad from every reading        -> global
		"a": {{ID: "x", Name: "X", RecentLossPct: f(100)}, {ID: "y", Name: "Y", RecentLossPct: f(100)}},
		"b": {{ID: "x", Name: "X", NoData: true}, {ID: "y", Name: "Y", RecentLossPct: f(100)}},
	}
	got := map[string]string{}
	for _, p := range analyzeTriage(byV) {
		got[p.Target] = p.Scope
	}
	if got["X"] != "vantage-specific" {
		t.Errorf("X (down+no_data): scope=%q, want vantage-specific", got["X"])
	}
	if got["Y"] != "global" {
		t.Errorf("Y (down everywhere): scope=%q, want global", got["Y"])
	}
}

// --- Fix 5: an unmatched vantage filter errors instead of widening ---

func TestTriageVantageNames(t *testing.T) {
	vs := []Vantage{{Name: "a"}, {Name: "b"}}

	got, err := triageVantageNames(vs, "")
	if err != nil || len(got) != 2 {
		t.Fatalf("empty filter: got=%v err=%v", got, err)
	}
	got, err = triageVantageNames(vs, "a")
	if err != nil || len(got) != 1 || got[0] != "a" {
		t.Fatalf("filter=a: got=%v err=%v", got, err)
	}
	if _, err := triageVantageNames(vs, "zzz"); err == nil {
		t.Error("filter=zzz (unknown): expected error, got nil")
	}
	got, err = triageVantageNames(nil, "")
	if err != nil || len(got) != 1 || got[0] != "" {
		t.Fatalf("no vantages, no filter: got=%v err=%v", got, err)
	}
}

// --- Fix 3: staging session-revision (refuse writes after reset; reset only if unchanged) ---

func TestStagingRefusesWriteAfterReset(t *testing.T) {
	c, st := stagedClient(t, `{"targets":{"children":{}}}`)
	if err := st.ensure(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	st.reset() // simulate a concurrent config_discard landing after ensure

	if err := stageAddTarget(st, addTargetIn{GroupPath: "g", Name: "a", Host: "1.1.1.1", Probe: "Ping"}); !errors.Is(err, errStaleStaging) {
		t.Errorf("stageAddTarget after reset: err=%v, want errStaleStaging", err)
	}
	yamlDoc := "targets:\n  children:\n    g:\n      children:\n        b:\n          host: 2.2.2.2\n          probe: Ping\n"
	if err := stageReplace(st, yamlDoc); !errors.Is(err, errStaleStaging) {
		t.Errorf("stageReplace after reset: err=%v, want errStaleStaging", err)
	}
}

func TestResetIfUnchangedPreservesNewerEdit(t *testing.T) {
	c, st := stagedClient(t, `{"targets":{"children":{}}}`)
	if err := st.ensure(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	_, _, seq0, ok := st.snapshotForApply()
	if !ok {
		t.Fatal("expected active session")
	}
	// A stage lands after the snapshot (bumps seq) — the older seq must not reset it.
	if err := stageAddTarget(st, addTargetIn{GroupPath: "g", Name: "a", Host: "1.1.1.1", Probe: "Ping"}); err != nil {
		t.Fatal(err)
	}
	if st.resetIfUnchanged(seq0) {
		t.Error("resetIfUnchanged(stale seq) reset the buffer, wiping a newer staged edit")
	}
	if !st.isActive() {
		t.Error("buffer was cleared despite a stale-seq reset")
	}
	// With the current seq it resets normally.
	_, _, seq1, _ := st.snapshotForApply()
	if !st.resetIfUnchanged(seq1) {
		t.Error("resetIfUnchanged(current seq) did not reset")
	}
	if st.isActive() {
		t.Error("buffer still active after a current-seq reset")
	}
}
