package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"smokeping-modern/internal/model"
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
