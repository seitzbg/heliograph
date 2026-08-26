package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"time"

	"github.com/seitzbg/heliograph/internal/agentwire"
	"github.com/seitzbg/heliograph/internal/federation"
	"github.com/seitzbg/heliograph/internal/model"
	"github.com/seitzbg/heliograph/internal/probe"
	"github.com/seitzbg/heliograph/internal/probe/ntpprobe"
	"github.com/seitzbg/heliograph/internal/sample"
	"github.com/seitzbg/heliograph/internal/scheduler"
	"github.com/seitzbg/heliograph/internal/store"
)

// agentAssignment serves the calling vantage its target list. The vantage identity
// comes from the authenticated request context (vantageFrom), never from a query
// param, so a caller can only ever fetch its own assignment. config_version is
// served as an ETag; an agent that replays it in If-None-Match gets 304.
func (srv *Server) agentAssignment(w http.ResponseWriter, r *http.Request) {
	v := vantageFrom(r)
	if v == store.DefaultVantage {
		http.Error(w, `{"error":"reserved vantage"}`, http.StatusForbidden)
		return
	}
	monitors, probeCfgs, cv := srv.Assignment(v)
	if match := r.Header.Get("If-None-Match"); match != "" && match == cv {
		w.Header().Set("ETag", cv)
		w.WriteHeader(http.StatusNotModified)
		return
	}
	out := agentwire.Assignment{Vantage: v, ConfigVersion: cv, Targets: make([]agentwire.AssignmentTarget, 0, len(monitors))}
	for _, m := range monitors {
		out.Targets = append(out.Targets, agentwire.AssignmentTarget{
			ID:   m.ID,
			Name: m.Name, Probe: m.ProbeKind, Host: m.Host,
			Params: m.Params, ProbeConfig: probeCfgs[m.ProbeKind],
			StepMs: m.Step.Milliseconds(), Pings: m.Pings,
			// Measurement-identity tag the agent echoes on each round; the hub
			// recomputes and verifies it on ingest (CODE_REVIEW #2).
			Fingerprint: federation.Fingerprint(m, probeCfgs[m.ProbeKind]),
		})
	}
	w.Header().Set("ETag", cv)
	writeJSON(w, out)
}

const maxIngestBytes = agentwire.MaxResultsBytes  // 16 MiB body cap (shared with the agent)
const maxIngestBatch = agentwire.MaxResultsRounds // rounds per request (shared with the agent)

// Ingest sanity bounds. An authenticated agent's samples are still validated: clock
// skew, an agent bug, or a forged/mismatched client cert could otherwise write a
// year-9999 row that stays "latest" forever, or inject negative/infinite/absurd
// latency that poisons the raw series and continuous aggregates (CODE_REVIEW #4 / P1-4).
const (
	// maxRTTSeconds bounds one received RTT. No real measurement waits an hour; a value
	// above this (or NaN/Inf/negative) is bogus and would distort the median/bands.
	maxRTTSeconds = 3600
	// maxDurationMs bounds a round's reported wall-clock duration (also 1 hour).
	maxDurationMs = 3600 * 1000
	// maxFutureSkew is how far ahead of the hub clock an ingested timestamp may be — a
	// small allowance for agent/hub clock drift. Beyond it the row is rejected, so a
	// far-future sample can never pin itself as the permanent "latest".
	maxFutureSkew = 5 * time.Minute
	// maxPastSkew is how old an ingested timestamp may be: generous enough for an agent
	// replaying an offline store-and-forward backlog, but bounded so ancient/forged
	// timestamps are rejected (older rounds fall outside raw retention anyway).
	maxPastSkew = 30 * 24 * time.Hour
)

// validSamples reports whether every sample and the duration are finite and within the documented
// magnitude bounds. A round with any bad value is dropped whole rather than silently partially
// accepted. RTT samples must be non-negative; a signed metric (offset-mode NTP) legitimately
// carries negative values, so signed=true allows them down to -maxRTTSeconds — the sign allowance
// is scoped to the authenticated assignment's metric, never the agent's self-report.
func validSamples(rtts []float64, durationMs float64, signed bool) bool {
	if math.IsNaN(durationMs) || math.IsInf(durationMs, 0) || durationMs < 0 || durationMs > maxDurationMs {
		return false
	}
	for _, v := range rtts {
		if math.IsNaN(v) || math.IsInf(v, 0) || v > maxRTTSeconds {
			return false
		}
		if signed {
			if v < -maxRTTSeconds {
				return false
			}
		} else if v < 0 {
			return false
		}
	}
	return true
}

// assignmentMetric is the metric an assigned target produces, per the authenticated assignment —
// the same probe.MetricFor resolution the hub's own scheduler and read API use, so a target reads
// the same metric wherever it is measured.
func assignmentMetric(m model.Monitor, probeCfgs map[string]map[string]string) string {
	return probe.MetricFor(m.ProbeKind, m.Params, probeCfgs[m.ProbeKind])
}

// withinSkew reports whether ts is within the accepted [now-maxPastSkew, now+maxFutureSkew]
// window relative to the hub clock.
func withinSkew(ts, now time.Time) bool {
	return !ts.After(now.Add(maxFutureSkew)) && !ts.Before(now.Add(-maxPastSkew))
}

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
		// An over-cap body is a PERMANENT rejection the agent must not retry forever — return
		// 413 (not the generic 400) so the agent can tell it apart and drop the batch (#2).
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			http.Error(w, `{"error":"request body too large"}`, http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, `{"error":"bad request"}`, http.StatusBadRequest)
		return
	}
	if len(req.Results) > maxIngestBatch {
		http.Error(w, `{"error":"batch too large"}`, http.StatusRequestEntityTooLarge)
		return
	}
	monitors, probeCfgs, _ := srv.Assignment(v)
	// allowed, wantFP, and metricByTarget are keyed by the target's stable id (the same id
	// the hub handed out on AssignmentTarget.ID and the agent echoes back as
	// RoundReport.Target) rather than its display Name/path, so a round is attributed to the
	// same storage identity the assignment named regardless of where the target currently
	// sits in the tree.
	allowed := make(map[string]model.Monitor, len(monitors))
	// wantFP is each assigned target's current measurement-identity fingerprint, computed
	// once per target here rather than per round: a store-and-forward replay is typically a
	// large batch spanning only a handful of distinct targets, so per-round hashing would
	// redo the same sha256 thousands of times.
	wantFP := make(map[string]string, len(monitors))
	metricByTarget := make(map[string]string, len(monitors))
	for _, m := range monitors {
		allowed[m.ID] = m
		wantFP[m.ID] = federation.Fingerprint(m, probeCfgs[m.ProbeKind])
		metricByTarget[m.ID] = assignmentMetric(m, probeCfgs)
	}
	// Rolling-upgrade fallback: a pre-#94 agent doesn't understand AssignmentTarget.ID and
	// reports RoundReport.Target = Name (its display path). Before a move ID==Name so the id-keyed
	// map already matches; after a move Name becomes the new path while ID stays the frozen birth
	// path, so such a round would be dropped as unassigned. byCurrentName lets an id-miss fall back
	// to the target's current display path.
	//
	// Collision handling: a target's display path can equal a DIFFERENT target's stable id (a
	// birth-path id that another target has since been created/moved onto). Such a token has two
	// candidate owners — the id owner (in `allowed`) and the current-path owner (recorded in
	// ambiguousOwner and kept OUT of byCurrentName so it is never a blind id-miss fallback). A bare
	// token can't tell "target X's stable id" from "target Y's current path", so the ingest loop
	// below resolves it by fingerprint (CODE_REVIEW M9):
	//   - a fingerprint-bearing round is attributed to whichever candidate its fingerprint UNIQUELY
	//     matches; if it matches both (identical host/probe/params) or neither, it stays unresolved;
	//   - a fingerprint-LESS round (a pre-fingerprint agent, which reports by path) is unattributable
	//     and is dropped rather than misattributed to the id owner.
	byCurrentName := make(map[string]model.Monitor, len(monitors))
	ambiguousName := map[string]bool{}
	ambiguousOwner := make(map[string]model.Monitor)
	for _, m := range monitors {
		if m.Name == m.ID {
			continue // id-keyed entry already covers this target
		}
		if _, isID := allowed[m.Name]; isID {
			ambiguousName[m.Name] = true // this display path is also another target's stable id
			ambiguousOwner[m.Name] = m   // the current-path owner (the id owner is allowed[name])
			continue
		}
		byCurrentName[m.Name] = m
	}
	outcomes := make([]scheduler.Outcome, 0, len(req.Results))
	dropped, mismatched, noFP := 0, 0, 0
	// Remote NTP stat updates are collected here and applied only AFTER the durable commit below,
	// and only for targets whose round was actually committed — so a round dropped by the commit-time
	// identity recheck (a reload landing between the snapshot validation above and the write) can't
	// publish a clock stat that was never stored (CODE_REVIEW M3). Keyed by stable id; the latest
	// round in the batch wins, preserving the previous last-write-wins behavior.
	type ntpUpdate struct {
		offsetSec float64
		stratum   uint8
		measure   string
		key       string    // the round's NTP measurement identity (host+port+version)
		ts        time.Time // the round's measurement time — the newest wins, in-batch and across
		clear     bool
	}
	pendingNTP := map[string]ntpUpdate{}
	now := time.Now()
	for _, rd := range req.Results {
		m, ok := allowed[rd.Target]
		if ok && ambiguousName[rd.Target] {
			// The token is both this target's stable id (X = m) and a different target's current
			// display path (Y = ambiguousOwner[token]). Resolve by fingerprint (CODE_REVIEW M9).
			y := ambiguousOwner[rd.Target]
			switch {
			case rd.Fingerprint == "":
				// Nothing to disambiguate the two candidates — can't be safely attributed to either,
				// so drop it (it also isn't in byCurrentName).
				ok = false
			case rd.Fingerprint == wantFP[m.ID] && rd.Fingerprint == wantFP[y.ID]:
				// Both candidates measure identically (same host/probe/params). Equivalent numbers
				// still belong to different graphs and the token can't say which, so drop rather than
				// guess — nothing on the wire proves which identity the agent meant.
				ok = false
			case rd.Fingerprint == wantFP[y.ID]:
				// The fingerprint uniquely identifies the current-path owner, not the id owner.
				m = y
				// default: the fingerprint matches the id owner alone, or neither. Keep m = X and let
				// the normal fingerprint gate below accept it (id match) or drop+count it (no match).
			}
		}
		if !ok {
			// Old-agent fallback: the round may be keyed by the target's current display path
			// rather than its stable id (CODE_REVIEW M9(B)).
			m, ok = byCurrentName[rd.Target]
		}
		if !ok {
			dropped++
			continue
		}
		ts, err := time.Parse(time.RFC3339Nano, rd.TS)
		// rd.Pings is bounded by the target's ASSIGNED pings (m.Pings), not the
		// global config ceiling. A legitimate agent measures with the assigned
		// count, so a larger self-reported pings is a bug or a hostile client
		// trying to make sample.Compute allocate an oversized per-round array
		// from a tiny request body (audit M1); m.Pings is already ≤ MaxPings.
		if err != nil || rd.Pings < 1 || rd.Pings > m.Pings || len(rd.RTTs) > rd.Pings {
			dropped++
			continue
		}
		metric := metricByTarget[m.ID] // keyed by stable id — rd.Target may be a display-path alias
		if !withinSkew(ts, now) || !validSamples(rd.RTTs, rd.DurationMs, metric == probe.MetricOffset) {
			dropped++
			continue
		}
		// Attribution: the round must have been measured under this target's CURRENT
		// identity. The hub recomputes the fingerprint from the live assignment and drops a
		// round whose target has since been redefined, so a buffered old measurement can't be
		// stored or alerted as the new target (CODE_REVIEW #2). An empty fingerprint comes from a
		// pre-fingerprint agent: in the default lenient mode it's accepted (counted) so a rolling
		// upgrade doesn't drop data; in strict mode (RequireFingerprint) it's a visible permanent
		// drop, since an unverifiable round could otherwise be misattributed across a redefinition.
		switch {
		case rd.Fingerprint == "":
			noFP++ // counted per vantage on /metrics either way
			if srv.RequireFingerprint {
				dropped++
				continue
			}
		case rd.Fingerprint != wantFP[m.ID]:
			dropped++
			mismatched++
			continue
		}
		o := scheduler.Outcome{
			// ID carries the stable storage identity (what the round is attributed/stored
			// under); Name carries the display path (what a panel/notification shows) —
			// looked up here from the id-keyed allowed map, so a remote round stores under
			// the same id local rounds do while still showing the target's current tree path.
			Target:    probe.Target{ID: m.ID, Name: m.Name, Host: m.Host},
			ProbeName: m.ProbeKind,
			Computed:  sample.Compute(rd.Pings, rd.RTTs),
			Metric:    metric, // from the authenticated assignment, not the agent's self-report
			When:      ts.UTC(),
			Duration:  time.Duration(rd.DurationMs * float64(time.Millisecond)),
			Vantage:   v,
			// Stamp the identity validated in THIS assignment snapshot even when a transitional
			// agent omitted its fingerprint. Missing-agent provenance is already counted separately;
			// carrying the snapshot fingerprint closes the reload-before-commit race for lenient
			// agents just as it does for current agents (CODE_REVIEW M2/M4).
			Fingerprint: wantFP[m.ID],
		}
		if rd.Err != "" {
			o.Err = errors.New(rd.Err)
		}
		outcomes = append(outcomes, o)
		// Record the NTP companion clock stat for this vantage, applied post-commit below. A
		// synchronized round reports offset + stratum; an unsynchronized/unreachable round, or a
		// pre-stat agent, reports neither, so the stat is cleared then — a remote NTP panel shows the
		// current reading or nothing, never a stale one (CODE_REVIEW M2). The update is tagged with the
		// round's measurement identity (host+port+version) and time, so a later read rejects it once
		// the target is repointed, and an out-of-order older round can't roll it back (M3). Within this
		// batch the newest round for a target wins; the registry then enforces the same across batches.
		if m.ProbeKind == "NTP" {
			var u ntpUpdate
			if rd.NTPOffsetMs != nil && rd.Stratum != nil && *rd.Stratum >= 0 && *rd.Stratum <= 255 {
				u = ntpUpdate{offsetSec: *rd.NTPOffsetMs / 1000, stratum: uint8(*rd.Stratum), measure: metric, key: ntpprobe.StatKey(m.Host, m.Params, probeCfgs["NTP"]), ts: ts}
			} else {
				u = ntpUpdate{clear: true, key: ntpprobe.StatKey(m.Host, m.Params, probeCfgs["NTP"]), ts: ts}
			}
			if prev, ok := pendingNTP[m.ID]; !ok || !u.ts.Before(prev.ts) {
				pendingNTP[m.ID] = u
			}
		}
	}
	accepted := len(outcomes)
	if len(outcomes) > 0 {
		// Persist + alert-evaluate the validated batch. When IngestCommit is wired (production
		// federation), it does both atomically under the runtime reload lock and re-validates each
		// outcome's target identity against the live assignment, so a reload landing between the
		// snapshot validation above and the write can't store a round under a since-redefined target
		// (CODE_REVIEW M4). Without it (pure API tests) fall back to a plain store write + OnIngest.
		// Either way alerts run only over the NEWLY persisted rounds: a replayed round — an HTTP retry
		// or the deliberate resend when a split batch's later half fails transiently — is deduplicated
		// by the store and excluded, so it can't re-advance alert hysteresis into a false FIRING or a
		// duplicate notification (CODE_REVIEW #4/replay). Each outcome carries its vantage (P2-5).
		var err error
		var committed []scheduler.Outcome
		if srv.IngestCommit != nil {
			committed, err = srv.IngestCommit(r.Context(), outcomes)
			if err == nil {
				accepted = len(committed)
				dropped += len(outcomes) - accepted
			}
		} else {
			if committed, err = ing.AddResults(r.Context(), outcomes); err == nil {
				if srv.OnIngest != nil && len(committed) > 0 {
					srv.OnIngest(committed)
				}
			}
		}
		if err != nil {
			slog.Error("agent ingest: write failed", "vantage", v, "err", err)
			http.Error(w, `{"error":"store unavailable"}`, http.StatusServiceUnavailable)
			return
		}
		// Apply the deferred remote NTP stat updates now, only for targets that were durably
		// committed — a round rejected by the commit-time identity recheck never touches the
		// displayed stat (CODE_REVIEW M3).
		for _, o := range committed {
			if u, ok := pendingNTP[o.Target.Key()]; ok {
				if u.clear {
					srv.remoteNTP.clear(v, o.Target.Key(), u.ts)
				} else {
					srv.remoteNTP.set(v, o.Target.Key(), u.offsetSec, u.stratum, u.measure, u.key, u.ts)
				}
				// Apply once per target even if the batch committed several of its rounds; the
				// entry already holds the newest (pendingNTP kept max ts above).
				delete(pendingNTP, o.Target.Key())
			}
		}
	}
	if dropped > 0 {
		// fingerprint_mismatch is broken out of the total so a redefined-target drop is
		// distinguishable from a not-in-assignment / malformed / stale-timestamp drop.
		slog.Warn("agent ingest: dropped rounds", "vantage", v, "dropped", dropped, "fingerprint_mismatch", mismatched)
	}
	if noFP > 0 {
		// Count per vantage (scrapeable on /metrics) and warn once per vantage, so an operator
		// can watch a rolling agent upgrade complete instead of relying on a single process-wide
		// log that goes silent for later-affected vantages (CODE_REVIEW #2).
		srv.recordMissingFingerprint(v, noFP)
	}
	writeJSON(w, agentwire.ResultsResponse{Accepted: accepted, Dropped: dropped})
}
