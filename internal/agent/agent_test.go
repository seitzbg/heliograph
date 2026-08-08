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
				// StepMs is huge (1h) so the Planner fires this target exactly ONCE
				// (new targets fire immediately; the next fire is ~1h out, well
				// outside the test window). With a short step the buffer keeps
				// refilling with fresh rounds every second, so "some round
				// eventually got through" would pass even if a FAILED push were
				// wrongly committed (a real data-loss bug) — a later fresh round
				// would paper over it. Firing once means the single measured round
				// can only reach `pushed` if the buffer RETAINED it across both 503s
				// and delivered it on the retry, which is the actual contract this
				// test exists to guard.
				Targets: []agentwire.AssignmentTarget{{Name: "t1", Probe: "AgentTestEcho", Host: "h", StepMs: 3600_000, Pings: 3}},
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
	// The window must comfortably cover the retry backoff: first push attempt
	// ~1-1.5s in (measure loop's own tick + flush loop's idle poll), then two
	// 503s at backoff ~1s then ~2s before the third attempt succeeds — done by
	// ~4-5s. 8s leaves ample margin. Run always blocks for the full window (all
	// three goroutines wait on ctx.Done()), so this is also the test's wall time.
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	_ = a.Run(ctx)

	mu.Lock()
	defer mu.Unlock()
	if len(pushed) == 0 {
		t.Fatal("agent never delivered the round despite retry — a failed push may have been committed (data loss)")
	}
	// Exactly one round was ever measured (the target fires once in this window),
	// so exactly one round may ever be pushed — commit-on-success delivers it once.
	if len(pushed) != 1 {
		t.Fatalf("expected exactly 1 delivered round, got %d: %+v", len(pushed), pushed)
	}
	got := pushed[0]
	if got.Target != "t1" || got.Pings != 3 || len(got.RTTs) != 3 {
		t.Fatalf("pushed round wrong: %+v", got)
	}
	if attempts < 3 {
		t.Fatalf("expected at least 3 push attempts (2 failures + success), got %d", attempts)
	}
}

// TestAgentRunTerminatesPromptlyOnCancel guards against a shutdown hang: after
// the fix that makes flushLoop wait for measureLoop's drain signal before its
// final flush (a bounded wait — see awaitMeasureDrain), Run must still return
// quickly when nothing is stuck (a healthy hub, a fast probe). This does not
// assert the internal drain ORDERING (that's a timing window, not something a
// deterministic test can pin without flaking); it asserts the externally
// observable, deterministic property that actually matters: Run doesn't hang.
func TestAgentRunTerminatesPromptlyOnCancel(t *testing.T) {
	const cv = "sha256:v3"
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
			_ = json.NewEncoder(w).Encode(agentwire.ResultsResponse{Accepted: len(req.Results)})
		}
	}))
	defer srv.Close()

	a := New(Options{Hub: srv.URL, Key: "smk_k_s", Vantage: "nyc", Interval: 50 * time.Millisecond,
		Timeout: time.Second, Workers: 4, BufferCap: 1000, FlushMax: 100})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = a.Run(ctx)
		close(done)
	}()

	time.Sleep(300 * time.Millisecond) // let poll + at least one measure tick happen
	cancelAt := time.Now()
	cancel()

	const hangBound = 5 * time.Second // drainWait(5s) + finalFlushTTL(5s) is the worst-case
	// bound with a stuck probe; a healthy hub + fast probe should return in
	// well under a second, so 5s has ample margin without being flaky.
	select {
	case <-done:
		if elapsed := time.Since(cancelAt); elapsed > hangBound {
			t.Fatalf("Run took too long to return after cancel: %s (possible shutdown hang)", elapsed)
		}
	case <-time.After(hangBound):
		t.Fatal("Run did not return within the bound after ctx cancellation — shutdown hang")
	}
}
