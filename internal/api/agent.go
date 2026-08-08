package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"smokeping-modern/internal/agentwire"
	"smokeping-modern/internal/config"
	"smokeping-modern/internal/model"
	"smokeping-modern/internal/probe"
	"smokeping-modern/internal/sample"
	"smokeping-modern/internal/scheduler"
	"smokeping-modern/internal/store"
)

// agentAssignment serves the calling vantage its target list. The vantage identity
// comes from the authenticated key (requireAgent), never from a query param, so a
// key can only ever fetch its own assignment. config_version is served as an ETag;
// an agent that replays it in If-None-Match gets 304.
func (srv *Server) agentAssignment(w http.ResponseWriter, r *http.Request) {
	v := vantageFrom(r)
	if v == store.DefaultVantage {
		http.Error(w, `{"error":"reserved vantage"}`, http.StatusForbidden)
		return
	}
	monitors, cv := srv.Assignment(v)
	if match := r.Header.Get("If-None-Match"); match != "" && match == cv {
		w.Header().Set("ETag", cv)
		w.WriteHeader(http.StatusNotModified)
		return
	}
	out := agentwire.Assignment{Vantage: v, ConfigVersion: cv, Targets: make([]agentwire.AssignmentTarget, 0, len(monitors))}
	for _, m := range monitors {
		out.Targets = append(out.Targets, agentwire.AssignmentTarget{
			Name: m.Name, Probe: m.ProbeKind, Host: m.Host,
			Params: m.Params, StepMs: m.Step.Milliseconds(), Pings: m.Pings,
		})
	}
	w.Header().Set("ETag", cv)
	writeJSON(w, out)
}

const maxIngestBytes = 16 << 20 // 16 MiB body cap
const maxIngestBatch = 5000     // rounds per request

// agentResults ingests a batch of measured rounds from the authenticated vantage.
// The hub is authoritative for probe/host (looked up in the current assignment);
// a target not in this vantage's assignment is dropped + counted + logged, never
// written. The agent sends raw received RTTs; the hub derives loss/median/centered
// via sample.Compute (one source of truth). A store write error answers 503 so the
// agent retains and retries.
func (srv *Server) agentResults(w http.ResponseWriter, r *http.Request) {
	v := vantageFrom(r)
	if v == store.DefaultVantage {
		http.Error(w, `{"error":"reserved vantage"}`, http.StatusForbidden)
		return
	}
	ing, ok := srv.store.(store.ResultIngester)
	if !ok {
		http.Error(w, `{"error":"ingest not supported by this store"}`, http.StatusNotImplemented)
		return
	}
	var req agentwire.ResultsRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxIngestBytes)).Decode(&req); err != nil {
		http.Error(w, `{"error":"bad request"}`, http.StatusBadRequest)
		return
	}
	if len(req.Results) > maxIngestBatch {
		http.Error(w, `{"error":"batch too large"}`, http.StatusRequestEntityTooLarge)
		return
	}
	monitors, _ := srv.Assignment(v)
	allowed := make(map[string]model.Monitor, len(monitors))
	for _, m := range monitors {
		allowed[m.Name] = m
	}
	outcomes := make([]scheduler.Outcome, 0, len(req.Results))
	dropped := 0
	for _, rd := range req.Results {
		m, ok := allowed[rd.Target]
		if !ok {
			dropped++
			continue
		}
		ts, err := time.Parse(time.RFC3339Nano, rd.TS)
		if err != nil || rd.Pings < 1 || rd.Pings > config.MaxPings || len(rd.RTTs) > rd.Pings {
			dropped++
			continue
		}
		o := scheduler.Outcome{
			Target:    probe.Target{Name: m.Name, Host: m.Host},
			ProbeName: m.ProbeKind,
			Computed:  sample.Compute(rd.Pings, rd.RTTs),
			When:      ts.UTC(),
			Duration:  time.Duration(rd.DurationMs * float64(time.Millisecond)),
			Vantage:   v,
		}
		if rd.Err != "" {
			o.Err = errors.New(rd.Err)
		}
		outcomes = append(outcomes, o)
	}
	if len(outcomes) > 0 {
		if err := ing.AddResults(r.Context(), outcomes); err != nil {
			slog.Error("agent ingest: write failed", "vantage", v, "err", err)
			http.Error(w, `{"error":"store unavailable"}`, http.StatusServiceUnavailable)
			return
		}
	}
	if dropped > 0 {
		slog.Warn("agent ingest: dropped rounds not in assignment", "vantage", v, "dropped", dropped)
	}
	writeJSON(w, agentwire.ResultsResponse{Accepted: len(outcomes), Dropped: dropped})
}
