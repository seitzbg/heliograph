package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"smokeping-modern/internal/agentwire"
)

func TestPullAssignment200And304(t *testing.T) {
	const cv = "sha256:v1"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer smk_k_s" {
			w.WriteHeader(401)
			return
		}
		if r.Header.Get("If-None-Match") == cv {
			w.Header().Set("ETag", cv)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", cv)
		_ = json.NewEncoder(w).Encode(agentwire.Assignment{
			Vantage: "nyc", ConfigVersion: cv,
			Targets: []agentwire.AssignmentTarget{{Name: "cf", Probe: "TCPConnect", Host: "127.0.0.1", StepMs: 1000, Pings: 1}},
		})
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "smk_k_s", false, 5*time.Second)

	asg, changed, err := c.PullAssignment(context.Background(), "")
	if err != nil || !changed || asg.ConfigVersion != cv || len(asg.Targets) != 1 {
		t.Fatalf("first pull: changed=%v err=%v asg=%+v", changed, err, asg)
	}
	_, changed, err = c.PullAssignment(context.Background(), cv)
	if err != nil || changed {
		t.Fatalf("304 pull: changed=%v err=%v (want changed=false, nil)", changed, err)
	}
}

func TestPullAssignmentUnauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(401) }))
	defer srv.Close()
	c := NewClient(srv.URL, "bad", false, 5*time.Second)
	if _, _, err := c.PullAssignment(context.Background(), ""); err == nil {
		t.Fatal("expected error on 401")
	}
}

func TestPushResults(t *testing.T) {
	var got agentwire.ResultsRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		_ = json.NewEncoder(w).Encode(agentwire.ResultsResponse{Accepted: len(got.Results), Dropped: 0})
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "smk_k_s", false, 5*time.Second)
	resp, err := c.PushResults(context.Background(), []agentwire.RoundReport{{Target: "cf", TS: "2026-08-07T00:00:00Z", Pings: 1, RTTs: []float64{0.01}}})
	if err != nil || resp.Accepted != 1 {
		t.Fatalf("push: resp=%+v err=%v", resp, err)
	}
	if len(got.Results) != 1 || got.Results[0].Target != "cf" {
		t.Fatalf("hub received %+v", got.Results)
	}
}

func TestPushResultsNon2xxErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer srv.Close()
	c := NewClient(srv.URL, "smk_k_s", false, 5*time.Second)
	if _, err := c.PushResults(context.Background(), []agentwire.RoundReport{{Target: "cf"}}); err == nil {
		t.Fatal("expected error on 503 so the caller retries")
	}
}
