package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/seitzbg/heliograph/internal/model"
	"github.com/seitzbg/heliograph/internal/probe"
	"github.com/seitzbg/heliograph/internal/sample"
	"github.com/seitzbg/heliograph/internal/scheduler"
	"github.com/seitzbg/heliograph/internal/store"
)

// TestMoveNodePreservesGraph is the end-to-end regression for the reported symptom: moving a
// target to a new parent group in the config tree must NOT lose its graph history. Tasks 1-9
// decouple a target's storage identity (a stable id, frozen at birth) from its display path,
// so a move only changes the catalog's id->path mapping, never the store key. This test proves
// that decoupling holds across the real HTTP surface: it configures a target at Resolvers/dns1
// (id == birth path, as the startup migration stamps it), ingests rounds, "moves" it by
// reconfiguring the SAME id under a new display path Datacenter/dns1, ingests one more round,
// and asserts the full history is reachable both by the frozen id/old path AND by the new
// display path (the exact request the browser sends after a move), and that /api/targets
// reports id and name correctly split.
func TestMoveNodePreservesGraph(t *testing.T) {
	const id = "Resolvers/dns1" // stable storage identity, frozen at birth == the pre-move path
	const oldPath = "Resolvers/dns1"
	const newPath = "Datacenter/dns1"

	st := store.NewMem(100)
	srv := New(st, "")
	srv.Active = func() map[string]bool { return map[string]bool{id: true} }

	// Step 1: configure the target pre-move. Display path and id coincide, as the startup
	// migration stamps id = birth path for every pre-existing node.
	srv.Configured = func() []model.Monitor {
		return []model.Monitor{{ID: id, Name: oldPath, ProbeKind: "FPing"}}
	}

	// Step 2: ingest several rounds under the identity (id-keyed, as a real probe/store round
	// is — Target.ID set, Target.Name empty, matching a pgstore-scanned outcome).
	base := time.Unix(1_700_000_000, 0)
	for i := 0; i < 3; i++ {
		st.Add([]scheduler.Outcome{{
			Target:    probe.Target{ID: id, Host: "10.0.0.1"},
			ProbeName: "FPing",
			Computed:  sample.Compute(2, []float64{0.01, 0.02}),
			When:      base.Add(time.Duration(i) * time.Minute),
		}})
	}

	getJSON := func(t *testing.T, url string, v any) int {
		t.Helper()
		rec := httptest.NewRecorder()
		srv.Routes().ServeHTTP(rec, httptest.NewRequest("GET", url, nil))
		if rec.Code == http.StatusOK {
			if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
				t.Fatalf("%s: decode: %v\nbody=%s", url, err, rec.Body.String())
			}
		}
		return rec.Code
	}

	type seriesResp struct {
		Target string           `json:"target"`
		Rounds []map[string]any `json:"rounds"`
	}

	// Pre-move sanity: the graph is reachable by its id (== the current path).
	var pre seriesResp
	if code := getJSON(t, "/api/series?target="+oldPath, &pre); code != http.StatusOK {
		t.Fatalf("pre-move /api/series status = %d, want 200", code)
	}
	if len(pre.Rounds) != 3 {
		t.Fatalf("pre-move rounds = %d, want 3", len(pre.Rounds))
	}

	// Step 3: the move — reconfigure so the SAME id now sits at a new display path. This is
	// exactly what editing the config tree and saving does: the id is preserved, only Name
	// (the path) changes.
	srv.Configured = func() []model.Monitor {
		return []model.Monitor{{ID: id, Name: newPath, ProbeKind: "FPing"}}
	}

	// Ingest one more round post-move, still keyed by the same identity — a moved target keeps
	// polling under its frozen id, unaffected by the display-path change.
	st.Add([]scheduler.Outcome{{
		Target:    probe.Target{ID: id, Host: "10.0.0.1"},
		ProbeName: "FPing",
		Computed:  sample.Compute(2, []float64{0.03, 0.04}),
		When:      base.Add(3 * time.Minute),
	}})

	// Assertion (a): the frozen id (== the old display path, since id == birth path) still
	// returns the FULL history — old rounds plus the post-move round.
	var byID seriesResp
	if code := getJSON(t, "/api/series?target="+id, &byID); code != http.StatusOK {
		t.Fatalf("(a) /api/series?target=%s status = %d, want 200", id, code)
	}
	if len(byID.Rounds) != 4 {
		t.Errorf("(a) FAILED: /api/series?target=%s (frozen id) rounds = %d, want 4 (full history lost across the move) — an identity seam still keys by path", id, len(byID.Rounds))
	}
	if byID.Target != id {
		t.Errorf("(a) target echo = %q, want %q", byID.Target, id)
	}

	// Assertion (b) [load-bearing]: the CURRENT display path — the exact request the browser
	// makes after a move, since the UI sends the node's live path — must ALSO return the full
	// history, via the catalog display-path->id fallback (resolveTargetKey). If this alone
	// fails, the frontend cannot rely on sending the current path and needs a follow-up fix.
	var byNewPath seriesResp
	if code := getJSON(t, "/api/series?target="+newPath, &byNewPath); code != http.StatusOK {
		t.Fatalf("(b) /api/series?target=%s status = %d, want 200", newPath, code)
	}
	if len(byNewPath.Rounds) != 4 {
		t.Errorf("(b) FAILED [load-bearing]: /api/series?target=%s (current display path, the browser's real request after a move) rounds = %d, want 4 — the display-path->id fallback has a gap; the frontend cannot rely on sending the current path", newPath, len(byNewPath.Rounds))
	}
	if byNewPath.Target != id {
		t.Errorf("(b) target echo = %q, want %q (resolved to the storage id)", byNewPath.Target, id)
	}

	// Assertion (c): /api/targets lists the target with id == the frozen birth path and
	// name == the current display path — the identity/display split holds after a move.
	var tj struct {
		Targets []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"targets"`
	}
	if code := getJSON(t, "/api/targets", &tj); code != http.StatusOK {
		t.Fatalf("(c) /api/targets status = %d, want 200", code)
	}
	if len(tj.Targets) != 1 {
		t.Fatalf("(c) want 1 target, got %d (%+v)", len(tj.Targets), tj.Targets)
	}
	if tj.Targets[0].ID != id {
		t.Errorf("(c) FAILED: id = %q, want %q (the frozen birth path)", tj.Targets[0].ID, id)
	}
	if tj.Targets[0].Name != newPath {
		t.Errorf("(c) FAILED: name = %q, want %q (the current display path)", tj.Targets[0].Name, newPath)
	}
}
