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

	"smokeping-modern/internal/model"
	"smokeping-modern/internal/scheduler"
	"smokeping-modern/internal/store"
)

func testAgentServer() *Server {
	asg := map[string][]model.Monitor{
		"nyc": {{Name: "cf", ProbeKind: "FPing", Host: "1.1.1.1", Pings: 20, Step: time.Minute, Vantages: []string{"nyc"}}},
	}
	return &Server{
		VantageAuth: fakeAuth{name: "nyc", ok: true},
		Assignment: func(v string) ([]model.Monitor, string) {
			return asg[v], "sha256:v1-" + v
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
			StepMs            int64 `json:"step_ms"`
			Pings             int
		} `json:"targets"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Vantage != "nyc" || len(got.Targets) != 1 || got.Targets[0].StepMs != 60000 || got.Targets[0].Host != "1.1.1.1" {
		t.Fatalf("unexpected assignment: %+v", got)
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
		Assignment: func(v string) ([]model.Monitor, string) {
			return []model.Monitor{{Name: "cf", ProbeKind: "FPing", Host: "1.1.1.1", Pings: 20, Step: time.Minute, Vantages: []string{"nyc"}}}, "sha256:v1"
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
		`{"results":[{"target":"cf","ts":"2026-08-07T12:00:00.5Z","pings":3,"rtts":[0.01,0.02,0.03]}]}`)
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
		`{"results":[{"target":"cf","ts":"2026-08-07T12:00:00Z","pings":1,"rtts":[0.01]}]}`)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d want 503", w.Code)
	}
}

// TestIngestDropsMalformedRounds covers the per-round validation in agentResults:
// each case is dropped (never reaches the store) without failing the whole batch.
func TestIngestDropsMalformedRounds(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"unparseable ts", `{"results":[{"target":"cf","ts":"not-a-time","pings":3,"rtts":[0.01]}]}`},
		{"pings below 1", `{"results":[{"target":"cf","ts":"2026-08-07T12:00:00Z","pings":0,"rtts":[]}]}`},
		{"pings above MaxPings", `{"results":[{"target":"cf","ts":"2026-08-07T12:00:00Z","pings":10001,"rtts":[0.01]}]}`},
		{"more rtts than pings", `{"results":[{"target":"cf","ts":"2026-08-07T12:00:00Z","pings":1,"rtts":[0.01,0.02]}]}`},
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
