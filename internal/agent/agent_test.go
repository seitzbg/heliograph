package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"smokeping-modern/internal/agentwire"
	"smokeping-modern/internal/probe"
)

func init() {
	// A deterministic, offline probe so the run loop is testable without network.
	probe.Register("AgentTestEcho", "test", map[string]probe.VarSpec{}, func(map[string]string) (probe.Probe, error) {
		return echoProbe{}, nil
	})
}

type echoProbe struct{}

func (echoProbe) Name() string { return "AgentTestEcho" }
func (echoProbe) Measure(_ context.Context, _ probe.Target, pings int) (probe.Result, error) {
	s := make([]float64, pings)
	for i := range s {
		s[i] = 0.01
	}
	return probe.Result{Samples: s}, nil
}

func TestAgentRunPullsMeasuresPushes(t *testing.T) {
	const cv = "sha256:v1"
	var (
		mu     sync.Mutex
		pushed []agentwire.RoundReport
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/agent/v1/assignment":
			if r.Header.Get("If-None-Match") == cv {
				w.WriteHeader(http.StatusNotModified)
				return
			}
			w.Header().Set("ETag", cv)
			_ = json.NewEncoder(w).Encode(agentwire.Assignment{
				Vantage: "nyc", ConfigVersion: cv,
				Targets: []agentwire.AssignmentTarget{{Name: "t1", Probe: "AgentTestEcho", Host: "h", StepMs: 1000, Pings: 3}},
			})
		case "/agent/v1/results":
			var req agentwire.ResultsRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			mu.Lock()
			pushed = append(pushed, req.Results...)
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(agentwire.ResultsResponse{Accepted: len(req.Results)})
		}
	}))
	defer srv.Close()

	a := New(Options{Hub: srv.URL, Key: "smk_k_s", Vantage: "nyc", Interval: 50 * time.Millisecond,
		Timeout: time.Second, Workers: 4, BufferCap: 1000, FlushMax: 100})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = a.Run(ctx)

	mu.Lock()
	defer mu.Unlock()
	if len(pushed) == 0 {
		t.Fatal("agent pushed nothing")
	}
	got := pushed[0]
	if got.Target != "t1" || got.Pings != 3 || len(got.RTTs) != 3 {
		t.Fatalf("pushed round wrong: %+v", got)
	}
}

// TestAgentRunRetriesOnPushFailure proves the flush loop retains a batch across a
// transient push failure (503) and eventually delivers it once the hub recovers —
// the retain+retry contract (do NOT commit on error).
func TestAgentRunRetriesOnPushFailure(t *testing.T) {
	const cv = "sha256:v2"
	var (
		mu       sync.Mutex
		pushed   []agentwire.RoundReport
		attempts int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/agent/v1/assignment":
			if r.Header.Get("If-None-Match") == cv {
				w.WriteHeader(http.StatusNotModified)
				return
			}
			w.Header().Set("ETag", cv)
			_ = json.NewEncoder(w).Encode(agentwire.Assignment{
				Vantage: "nyc", ConfigVersion: cv,
				Targets: []agentwire.AssignmentTarget{{Name: "t1", Probe: "AgentTestEcho", Host: "h", StepMs: 1000, Pings: 3}},
			})
		case "/agent/v1/results":
			mu.Lock()
			attempts++
			n := attempts
			mu.Unlock()
			if n <= 2 {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			var req agentwire.ResultsRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			mu.Lock()
			pushed = append(pushed, req.Results...)
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(agentwire.ResultsResponse{Accepted: len(req.Results)})
		}
	}))
	defer srv.Close()

	a := New(Options{Hub: srv.URL, Key: "smk_k_s", Vantage: "nyc", Interval: 50 * time.Millisecond,
		Timeout: time.Second, Workers: 4, BufferCap: 1000, FlushMax: 100})
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	_ = a.Run(ctx)

	mu.Lock()
	defer mu.Unlock()
	if len(pushed) == 0 {
		t.Fatal("agent never delivered rounds despite retry")
	}
	if attempts < 3 {
		t.Fatalf("expected at least 3 push attempts (2 failures + success), got %d", attempts)
	}
}
