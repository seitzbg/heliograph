package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
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

// Options.Validate is the package boundary that stops a non-CLI caller from building a
// dangerous agent — notably FlushMax<1, which panics peekBatch (CODE_REVIEW #9 / P2-9).
func TestOptionsValidate(t *testing.T) {
	ok := Options{Hub: "https://h", Key: "k", Interval: time.Minute, Timeout: time.Second, Workers: 1, BufferCap: 1, FlushMax: 1}
	if err := ok.Validate(); err != nil {
		t.Fatalf("valid options rejected: %v", err)
	}
	bad := func(f func(*Options)) Options { o := ok; f(&o); return o }
	cases := map[string]Options{
		"no hub":                       bad(func(o *Options) { o.Hub = "" }),
		"no key":                       bad(func(o *Options) { o.Key = "" }),
		"zero interval":                bad(func(o *Options) { o.Interval = 0 }),
		"neg timeout":                  bad(func(o *Options) { o.Timeout = -1 }),
		"zero workers":                 bad(func(o *Options) { o.Workers = 0 }),
		"zero buffer":                  bad(func(o *Options) { o.BufferCap = 0 }),
		"zero flushmax":                bad(func(o *Options) { o.FlushMax = 0 }),
		"neg flushmax":                 bad(func(o *Options) { o.FlushMax = -1 }),
		"flushmax too big (> hub cap)": bad(func(o *Options) { o.FlushMax = agentwire.MaxResultsRounds + 1 }),
	}
	for name, o := range cases {
		if err := o.Validate(); err == nil {
			t.Errorf("%s: expected a validation error, got nil", name)
		}
	}
}

// finalFlush must drain the WHOLE buffer at shutdown, looping over FlushMax batches,
// not stop after one — otherwise a controlled shutdown after a hub outage strands every
// buffered round past the first batch (CODE_REVIEW #8 / P2-8).
func TestFinalFlushDrainsAllBatches(t *testing.T) {
	var (
		mu      sync.Mutex
		rounds  int
		batches int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/agent/v1/results" {
			http.NotFound(w, r)
			return
		}
		var req agentwire.ResultsRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		rounds += len(req.Results)
		batches++
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(agentwire.ResultsResponse{Accepted: len(req.Results)})
	}))
	defer srv.Close()

	a := New(Options{Hub: srv.URL, Key: "smk_k_s", Vantage: "nyc",
		Timeout: time.Second, Workers: 1, BufferCap: 1000, FlushMax: 10})
	// Buffer 25 rounds -> 3 batches of {10,10,5} must all be delivered.
	for i := 0; i < 25; i++ {
		a.buf.add(agentwire.RoundReport{Target: "t1", Pings: 1, RTTs: []float64{0.01}})
	}

	a.finalFlush(5 * time.Second)

	if n := a.buf.len(); n != 0 {
		t.Errorf("buffer not drained: %d rounds left", n)
	}
	mu.Lock()
	defer mu.Unlock()
	if rounds != 25 {
		t.Errorf("hub received %d rounds, want 25 (final flush stopped early)", rounds)
	}
	if batches < 3 {
		t.Errorf("hub received %d batches, want >=3 (loop over FlushMax batches)", batches)
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

// captureHandler is a concurrency-safe slog.Handler that records emitted records
// so a test can assert on log output.
type captureHandler struct {
	mu      *sync.Mutex
	records *[]slog.Record
}

func (h captureHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h captureHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	*h.records = append(*h.records, r.Clone())
	return nil
}
func (h captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h captureHandler) WithGroup(string) slog.Handler      { return h }

// The hub derives the vantage from the API key; the configured `vantage` is only a
// label. When they disagree, the agent must surface the mismatch so its rounds can't
// be silently attributed to the wrong vantage.
func TestPollLoopWarnsOnVantageMismatch(t *testing.T) {
	var mu sync.Mutex
	var records []slog.Record
	prev := slog.Default()
	slog.SetDefault(slog.New(captureHandler{mu: &mu, records: &records}))
	defer slog.SetDefault(prev)

	const cv = "sha256:v1"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/agent/v1/assignment":
			w.Header().Set("ETag", cv)
			_ = json.NewEncoder(w).Encode(agentwire.Assignment{
				Vantage: "nyc", ConfigVersion: cv,
				Targets: []agentwire.AssignmentTarget{{Name: "t1", Probe: "AgentTestEcho", Host: "h", StepMs: 1000, Pings: 1}},
			})
		case "/agent/v1/results":
			_ = json.NewEncoder(w).Encode(agentwire.ResultsResponse{Accepted: 0})
		}
	}))
	defer srv.Close()

	// Configured "lax", but the hub (key-derived) assigns "nyc": expect a warning naming both.
	a := New(Options{Hub: srv.URL, Key: "smk_k_s", Vantage: "lax", Interval: 50 * time.Millisecond,
		Timeout: time.Second, Workers: 2, BufferCap: 100, FlushMax: 10})
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	_ = a.Run(ctx)

	mu.Lock()
	defer mu.Unlock()
	found := false
	for _, r := range records {
		if r.Level != slog.LevelWarn {
			continue
		}
		var configured, assigned string
		r.Attrs(func(at slog.Attr) bool {
			switch at.Key {
			case "configured":
				configured = at.Value.String()
			case "assigned":
				assigned = at.Value.String()
			}
			return true
		})
		if configured == "lax" && assigned == "nyc" {
			found = true
		}
	}
	if !found {
		t.Fatal("expected a warning naming the configured (lax) and assigned (nyc) vantages")
	}
}

// CODE_REVIEW #2: a permanently-rejected batch (413) must be DROPPED (counted) so the queue
// drains, not retried forever which would wedge every newer round behind it.
func TestFlushLoopDropsPermanentlyRejectedBatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusRequestEntityTooLarge) // 413: the hub will never accept it
	}))
	defer srv.Close()
	a := New(Options{Hub: srv.URL, Key: "smk_k_s", Vantage: "nyc",
		Timeout: time.Second, Workers: 1, BufferCap: 1000, FlushMax: 10})
	for i := 0; i < 5; i++ {
		a.buf.add(agentwire.RoundReport{Target: "t1", Pings: 1, RTTs: []float64{0.01}})
	}
	close(a.measureDone) // so shutdown()'s awaitMeasureDrain returns immediately on cancel
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { a.flushLoop(ctx); close(done) }()
	deadline := time.Now().Add(3 * time.Second)
	for a.buf.len() != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	if n := a.buf.len(); n != 0 {
		t.Fatalf("permanently-rejected batch not dropped: %d rounds still buffered", n)
	}
	if got := a.buf.rejected(); got < 5 {
		t.Fatalf("rejected counter = %d, want >= 5", got)
	}
}

// CODE_REVIEW round-9 #1: a 413 is a recoverable SIZE condition, not a "this batch is
// unsendable" verdict — most of the batch may be perfectly valid, individually-sendable
// rounds sitting next to one oversized round. The fix must SPLIT an oversized batch and
// isolate the unsendable round(s), instead of dropping every round in the batch wholesale.
//
// The fake hub below mirrors the real one (internal/api/agent.go agentResults): it 413s any
// request whose marshaled body exceeds a byte cap, and accepts anything under it. The cap is
// sized well below agentwire.MaxResultsBytes (16 MiB) because a single round can't
// realistically exceed 16 MiB under sane ping counts — per the review's own note, a smaller
// fake cap is used so a single round can representably exceed it. The test validates its own
// instrument (asserts the huge round's marshaled size actually exceeds the fake cap) before
// trusting the result, per the "validate the probe" rule: a cap the huge round doesn't
// actually exceed would make this test pass for the wrong reason.
func TestFinalFlushSplitsOversizedBatchInsteadOfDroppingIt(t *testing.T) {
	const fixedTS = "2026-08-07T00:00:00Z"
	ordinary := func(name string) agentwire.RoundReport {
		return agentwire.RoundReport{Target: name, TS: fixedTS, Pings: 1, RTTs: []float64{0.01}}
	}
	huge := agentwire.RoundReport{Target: "huge", TS: fixedTS, Pings: 500, RTTs: make([]float64, 500)}
	for i := range huge.RTTs {
		huge.RTTs[i] = 0.012345 + float64(i)/1e6
	}

	ordinaryBody, err := json.Marshal(agentwire.ResultsRequest{Results: []agentwire.RoundReport{ordinary("t")}})
	if err != nil {
		t.Fatalf("marshal ordinary round: %v", err)
	}
	hugeBody, err := json.Marshal(agentwire.ResultsRequest{Results: []agentwire.RoundReport{huge}})
	if err != nil {
		t.Fatalf("marshal huge round: %v", err)
	}
	const ordinaryBudget = 20 // fake cap fits ~20 batched ordinary rounds, but not the huge one
	fakeCap := len(ordinaryBody)*ordinaryBudget + 64
	if fakeCap >= len(hugeBody) {
		t.Fatalf("test setup invalid: huge round (%d bytes) does not exceed fake cap (%d bytes) — grow the huge round's RTTs", len(hugeBody), fakeCap)
	}

	var (
		mu       sync.Mutex
		received []agentwire.RoundReport
		requests int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if len(body) > fakeCap {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			return
		}
		var req agentwire.ResultsRequest
		_ = json.Unmarshal(body, &req)
		mu.Lock()
		received = append(received, req.Results...)
		requests++
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(agentwire.ResultsResponse{Accepted: len(req.Results)})
	}))
	defer srv.Close()

	a := New(Options{Hub: srv.URL, Key: "smk_k_s", Vantage: "nyc",
		Timeout: time.Second, Workers: 1, BufferCap: 1000, FlushMax: 100})

	const numOrdinary = 50
	a.buf.add(huge)
	for i := 0; i < numOrdinary; i++ {
		a.buf.add(ordinary(fmt.Sprintf("t%d", i)))
	}

	a.finalFlush(5 * time.Second)

	if n := a.buf.len(); n != 0 {
		t.Fatalf("buffer not drained (wedged): %d rounds still buffered", n)
	}
	mu.Lock()
	gotReceived := len(received)
	gotRequests := requests
	mu.Unlock()
	if gotReceived != numOrdinary {
		t.Errorf("hub received %d rounds, want exactly %d ordinary rounds (the split must isolate and drop only the huge one, not the whole batch)", gotReceived, numOrdinary)
	}
	for _, r := range received {
		if r.Target == "huge" {
			t.Error("the oversized round reached the hub — it alone exceeds the fake cap and can never be sendable")
		}
	}
	if got := a.buf.rejected(); got != 1 {
		t.Errorf("rejected = %d, want exactly 1 (only the single unsendable huge round)", got)
	}
	// Sanity bound: 51 rounds split by binary halving cannot need more than a few hundred
	// requests. A number in that range instead would point at non-terminating/runaway
	// recursion rather than a bounded split — the "split terminates" assertion the review asked
	// for, made concrete rather than just "the test finished".
	if gotRequests > 500 {
		t.Errorf("hub received %d requests for %d rounds — suspiciously many, possible non-terminating split", gotRequests, numOrdinary+1)
	}
}

// A malformed (400) batch must be dropped WHOLESALE, unsplit — unlike a 413, a 400 means the
// hub's decoder rejected the shape of the request, and no sub-batch of the same shape is any
// more acceptable, so splitting would just waste requests without recovering anything.
func TestSendBatchDropsMalformedBatchWholesaleWithoutSplitting(t *testing.T) {
	var requests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()
	a := New(Options{Hub: srv.URL, Key: "smk_k_s", Vantage: "nyc",
		Timeout: time.Second, Workers: 1, BufferCap: 100, FlushMax: 10})

	batch := make([]agentwire.RoundReport, 5)
	for i := range batch {
		batch[i] = agentwire.RoundReport{Target: fmt.Sprintf("t%d", i), Pings: 1, RTTs: []float64{0.01}}
	}
	sent, dropped, err := a.sendBatch(context.Background(), batch)
	if err != nil {
		t.Fatalf("sendBatch returned err=%v, want nil (400 is handled, not propagated)", err)
	}
	if sent != 0 || dropped != len(batch) {
		t.Errorf("sent=%d dropped=%d, want sent=0 dropped=%d (whole batch dropped)", sent, dropped, len(batch))
	}
	if requests != 1 {
		t.Errorf("hub received %d requests, want exactly 1 (a 400 must not trigger a split)", requests)
	}
}

// A transient (5xx) failure must propagate as an error — never split, never dropped — so the
// caller retains and retries the WHOLE batch later, exactly the pre-existing transient-retry
// contract this fix must not disturb.
func TestSendBatchPropagatesTransientErrorWithoutSplittingOrDropping(t *testing.T) {
	var requests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	a := New(Options{Hub: srv.URL, Key: "smk_k_s", Vantage: "nyc",
		Timeout: time.Second, Workers: 1, BufferCap: 100, FlushMax: 10})

	batch := make([]agentwire.RoundReport, 5)
	for i := range batch {
		batch[i] = agentwire.RoundReport{Target: fmt.Sprintf("t%d", i), Pings: 1, RTTs: []float64{0.01}}
	}
	sent, dropped, err := a.sendBatch(context.Background(), batch)
	if err == nil {
		t.Fatal("sendBatch returned err=nil for a 503, want a propagated error so the caller retries")
	}
	if sent != 0 || dropped != 0 {
		t.Errorf("sent=%d dropped=%d, want both 0 (transient failure hands nothing back but the error)", sent, dropped)
	}
	if requests != 1 {
		t.Errorf("hub received %d requests, want exactly 1 (a transient failure must not trigger a split)", requests)
	}
}

// CODE_REVIEW round-9 whole-branch review #1 (deferred minor): the split path was only ever
// traced by hand for the case where a 413 splits a batch, the FIRST half POSTs fine, and the
// SECOND half then hits a TRANSIENT (5xx) error — sendBatch must discard the first half's
// already-successful send-count and propagate the error so the caller (flushLoop) retries the
// WHOLE original batch, not just the second half, and does not double-count or lose anything in
// the process. This is the first committed test for that interleaving; previously it was only
// verified by trace.
//
// The fake hub caps a request at capN rounds (413 above it, mirroring the real hub's byte cap
// applied to round count for a deterministic, easy-to-reason-about split point) and, for the
// specific sub-batch that sendBatch's split produces as the SECOND half, answers 503 on its
// first appearance only — every later appearance (the retry, and any duplicate resend of the
// first half) succeeds, exactly like a hub that had a transient blip and recovered.
func TestSendBatchTransientOnSecondSplitHalfRetriesWholeBatchWithoutLoss(t *testing.T) {
	const capN = 5 // hub's fake per-request round cap
	const total = 9
	rounds := make([]agentwire.RoundReport, total)
	for i := range rounds {
		rounds[i] = agentwire.RoundReport{Target: fmt.Sprintf("r%d", i), Pings: 1, RTTs: []float64{0.01}}
	}
	// Mirrors sendBatch's own split point (mid := len(rounds) / 2) so the fake hub can
	// recognize which sub-batch is "the second half" without reaching into sendBatch itself.
	mid := total / 2
	keyOf := func(results []agentwire.RoundReport) string {
		names := make([]string, len(results))
		for i, r := range results {
			names[i] = r.Target
		}
		return strings.Join(names, ",")
	}
	secondHalfKey := keyOf(rounds[mid:])

	var (
		mu       sync.Mutex
		attempts = map[string]int{}
		received []agentwire.RoundReport
		requests int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req agentwire.ResultsRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		requests++
		mu.Unlock()
		if len(req.Results) > capN {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			return
		}
		key := keyOf(req.Results)
		mu.Lock()
		attempts[key]++
		n := attempts[key]
		mu.Unlock()
		if key == secondHalfKey && n == 1 {
			w.WriteHeader(http.StatusServiceUnavailable) // transient, first attempt only
			return
		}
		mu.Lock()
		received = append(received, req.Results...)
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(agentwire.ResultsResponse{Accepted: len(req.Results)})
	}))
	defer srv.Close()

	a := New(Options{Hub: srv.URL, Key: "smk_k_s", Vantage: "nyc",
		Timeout: time.Second, Workers: 1, BufferCap: 1000, FlushMax: 20})
	for _, r := range rounds {
		a.buf.add(r)
	}
	close(a.measureDone) // so shutdown()'s awaitMeasureDrain returns immediately on cancel

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { a.flushLoop(ctx); close(done) }()

	// The first cycle: whole batch 413s, splits, the first half POSTs OK, the second half
	// hits the transient 503. That must NOT commit anything — give it time to run (well
	// within flushLoop's 1s backoff before it retries) and confirm the whole batch is still
	// buffered, and nothing was miscounted as rejected.
	time.Sleep(200 * time.Millisecond)
	if n := a.buf.len(); n != total {
		t.Fatalf("buffer len = %d after the transient failure, want %d still buffered — "+
			"a transient error on the second split half must leave the WHOLE peeked batch "+
			"uncommitted (including the first half's already-successful send), not just its own half",
			n, total)
	}
	if got := a.buf.rejected(); got != 0 {
		t.Fatalf("rejected = %d after the transient failure, want 0 (nothing here is a permanent rejection)", got)
	}

	// Wait for the retry (after ~1s backoff) to succeed and drain the buffer.
	deadline := time.Now().Add(5 * time.Second)
	for a.buf.len() != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done

	if n := a.buf.len(); n != 0 {
		t.Fatalf("buffer not drained after the retry: %d round(s) still buffered", n)
	}
	if got := a.buf.rejected(); got != 0 {
		t.Fatalf("rejected = %d, want 0 (every round was eventually sendable; none may be counted rejected)", got)
	}

	mu.Lock()
	defer mu.Unlock()
	seen := map[string]int{}
	for _, r := range received {
		seen[r.Target]++
	}
	for _, r := range rounds {
		if seen[r.Target] == 0 {
			t.Errorf("round %q never reached the hub — lost by the agent", r.Target)
		}
	}
	// The retried first half may legitimately reach the hub twice (dedup on
	// (target,vantage,ts) is the hub's job, not the agent's), but every round must reach it
	// AT LEAST once and the agent's own bookkeeping (buffer drained, rejected==0) must not
	// depend on how many times the hub actually saw it.
	if len(received) < total {
		t.Errorf("hub received %d round-delivery(ies) total, want at least %d (one per round)", len(received), total)
	}
}
