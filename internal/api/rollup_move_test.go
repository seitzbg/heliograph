package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/seitzbg/heliograph/internal/model"
	"github.com/seitzbg/heliograph/internal/probe"
	"github.com/seitzbg/heliograph/internal/store"
)

// rollupSpyStore embeds a MemStore (so it satisfies the base Store and the read interfaces the
// server wires) and adds a Rollup method — MemStore alone is NOT a store.Rollupper, so the rollup
// handler would otherwise 501 before ever resolving the key. The spy records the target key it was
// queried with and returns canned buckets ONLY for the frozen storage id it was seeded with, mirroring
// a real store that has rows under a target's id but none under its post-move display path.
type rollupSpyStore struct {
	*store.MemStore
	dataID    string // the frozen storage id that actually "has" rollup rows
	gotTarget string // the last target key Rollup was queried with
}

func (s *rollupSpyStore) Rollup(_ context.Context, target, _ /*vantage*/, _ /*resolution*/ string, _, _ time.Time) ([]store.RollupPoint, error) {
	s.gotTarget = target
	if target != s.dataID {
		// A store holds no rollup rows under a moved target's DISPLAY PATH — only under its id.
		return nil, nil
	}
	return []store.RollupPoint{{
		Bucket:       time.Unix(1_700_000_000, 0),
		MedianAvg:    0.015,
		MedianMin:    0.01,
		MedianMax:    0.02,
		LossFrac:     0,
		Rounds:       2,
		MedianRounds: 2,
		Metric:       probe.MetricRTT,
	}}, nil
}

// TestRollupResolvesMovedTargetPath is the regression for the second read path a moved target
// depends on: /api/rollup (the drill-down bands and drag-zoom) must resolve the current display
// path to the frozen storage id, exactly as /api/series does. The frontend sends the node's LIVE
// path; for a moved target (id != path) the store only has rows under the id, so without
// resolveTargetKey the handler queries the display path, gets nothing, and every band tier and
// zoomed view renders blank. The spy returns data only for the frozen id, so this fails (empty
// buckets) until the handler resolves the key.
func TestRollupResolvesMovedTargetPath(t *testing.T) {
	const id = "Resolvers/dns1"       // stable storage identity, frozen at birth == pre-move path
	const newPath = "Datacenter/dns1" // current display path after the move

	spy := &rollupSpyStore{MemStore: store.NewMem(100), dataID: id}
	srv := New(spy, "")
	srv.Active = func() map[string]bool { return map[string]bool{id: true} }
	// The moved target: same id, new display path — what saving a config-tree move produces.
	srv.Configured = func() []model.Monitor {
		return []model.Monitor{{ID: id, Name: newPath, ProbeKind: "FPing"}}
	}

	// The exact request the browser makes after a move: the node's live display path.
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, httptest.NewRequest("GET", "/api/rollup?target="+newPath+"&res=1h&window=240h", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/api/rollup?target=%s status = %d, want 200\nbody=%s", newPath, rec.Code, rec.Body.String())
	}

	var resp struct {
		Target  string           `json:"target"`
		Buckets []map[string]any `json:"buckets"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v\nbody=%s", err, rec.Body.String())
	}

	// Load-bearing: the store must have been queried by the FROZEN id, not the display path.
	if spy.gotTarget != id {
		t.Errorf("FAILED: /api/rollup queried the store with target %q, want %q (the resolved storage id) — the display-path->id resolution has a gap", spy.gotTarget, id)
	}
	// And so the moved target's rollup data comes back non-empty (blank bands were the symptom).
	if len(resp.Buckets) == 0 {
		t.Errorf("FAILED: /api/rollup?target=%s (current display path) returned no buckets — a moved target's drill-down bands render blank", newPath)
	}
	if resp.Target != id {
		t.Errorf("target echo = %q, want %q (resolved to the storage id)", resp.Target, id)
	}
}
