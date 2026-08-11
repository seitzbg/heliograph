// Package api exposes the collected series as JSON — the modern replacement for
// SmokePing's CGI (see codemap 05 §7). The frontend SPA would render the smoke
// graph client-side from /api/series. NaN medians serialize as null.
package api

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"smokeping-modern/internal/model"
	"smokeping-modern/internal/probe"
	"smokeping-modern/internal/scheduler"
	"smokeping-modern/internal/store"
	"smokeping-modern/internal/vantage"
)

// VantageAdmin is the subset of the vantage key store the admin API uses. Kept an
// interface so the handlers test with a fake and api needs no live DB.
type VantageAdmin interface {
	Add(ctx context.Context, name string) (fullKey string, err error)
	List(ctx context.Context) ([]vantage.Info, error)
	Revoke(ctx context.Context, name string) (removed bool, err error)
}

// ErrConfigInvalid and ErrConfigConflict are the sentinel results a ConfigApply
// implementation returns; putConfig maps them to 400 and 409. Defined here so the
// api package needs no dependency on internal/configstore (main translates).
var (
	ErrConfigInvalid  = errors.New("config: invalid")
	ErrConfigConflict = errors.New("config: version conflict")
)

type Server struct {
	store  store.Store
	webDir string
	// Rounds, if set, adds collector round-level metrics (duration, size, error
	// count) to /metrics. Optional; nil in tests and pure-API use.
	Rounds *RoundStats
	// Active, if set, returns the set of currently-configured target names across
	// ALL vantages (not just the hub's locally-probed jobs), so a remote-only
	// target's stored rows survive the filter for its own vantage (CODE_REVIEW #3 /
	// P1-3). The live endpoints (targets, charts, sla, metrics) filter the store's
	// keys through it, so a target removed or renamed on a config reload stops being
	// reported as healthy — its historical rows remain in the store but are no
	// longer surfaced in the live views. nil means no filtering (tests/pure API).
	Active func() map[string]bool
	// Steps, if set, returns each configured target's polling step. /api/sla uses
	// it to compute how many rounds a window should have contained (expected), and
	// thus coverage. A target with no known step reports availability without
	// coverage. nil means coverage is omitted entirely.
	Steps func() map[string]time.Duration
	// ExtraMetrics, if set, appends extra Prometheus lines to /metrics after the
	// probe and round metrics — the webhook notifier wires its delivery counters here
	// (a plain func so api needs no dependency on the alert package). nil = none.
	ExtraMetrics func(*strings.Builder)
	// Vantages, AdminPassword, AdminKey enable the admin key-management API. All three
	// must be set (Vantages requires -dsn; AdminPassword is SMOKED_ADMIN_PASSWORD; AdminKey
	// is a random per-process HMAC key). If AdminPassword is "" the admin routes are not
	// registered at all — fail-closed.
	Vantages      VantageAdmin
	AdminPassword string
	AdminKey      []byte
	// TargetVantages, if set, returns each configured target's assigned vantage set (name
	// -> vantage names) — /api/targets surfaces it so the UI can show which vantages a
	// target is measured from. nil means the field is omitted (tests/pure API/no federation).
	TargetVantages func() map[string][]string
	// Configured, if set, returns the full configured target catalog (all vantages), so
	// /api/targets lists a target even when it has no stored row for the requested vantage
	// yet — e.g. a remote-only target the hub never probes locally (CODE_REVIEW #3 / P1-3).
	// Without it, /api/targets shows only targets that already have a latest row, hiding
	// remote-only targets from the tree and their deep links. nil = latest-only listing.
	Configured func() []model.Monitor
	// VantageAuth, if set, gates the agent routes (requireAgent) behind an API-key check
	// against the vantage key store. nil means the agent routes are not registered at
	// all — fail-closed, same pattern as the admin API's AdminPassword gate.
	VantageAuth VantageAuth
	// Assignment, if set, returns the target list, the effective probe-level config
	// (probe kind -> its `probes.<Kind>` block), and a config_version for a vantage,
	// computed over the live monitor set. Required (with VantageAuth) for the agent routes.
	Assignment func(vantage string) (targets []model.Monitor, probeCfgs map[string]map[string]string, configVersion string)
	// OnIngest, if set, is called with the accepted remote outcomes AFTER they are durably
	// stored, so the hub evaluates alerts for remote vantages (each outcome carries its
	// vantage) — the post-ingest hook that closes CODE_REVIEW #5 / P2-5. nil = no alerting
	// on ingest (e.g. pure API tests).
	OnIngest func(outcomes []scheduler.Outcome)
	// RequireFingerprint, when true, makes agent ingest STRICT: a round with no measurement
	// fingerprint (a pre-fingerprint agent) is dropped as a visible permanent drop instead of
	// accepted. Default false keeps the lenient/compatible behavior (accepted + counted) so a
	// rolling agent upgrade doesn't lose data; an operator flips this on once the per-vantage
	// missing-fingerprint metric shows every agent upgraded (CODE_REVIEW #2).
	RequireFingerprint bool
	// ingestMu guards the agent-ingest observability counters below.
	ingestMu sync.Mutex
	// missingFP counts, per vantage, agent rounds received with no measurement fingerprint (a
	// pre-fingerprint agent) — accepted-unverified in the lenient default, or dropped in strict
	// mode (-require-fingerprint) (CODE_REVIEW #2). Exposed on /metrics
	// (smokeping_agent_missing_fingerprint_total) so an operator can watch a rolling agent
	// upgrade complete — the counter stops rising — rather than relying on a single
	// process-wide log line that goes silent for later-affected vantages.
	missingFP map[string]int64
	// warnedMissingFP debounces the missing-fingerprint warning to once per vantage.
	warnedMissingFP map[string]bool
	// ConfigGet/ConfigApply enable the DB config CRUD API (GET/PUT /api/admin/config),
	// gated behind the admin password. Both nil ⇒ the routes are not registered.
	// ConfigApply validates the candidate config, persists it, and hot-reloads —
	// returning ErrConfigInvalid (⇒400) or ErrConfigConflict (⇒409).
	ConfigGet   func() (doc json.RawMessage, version int, err error)
	ConfigApply func(doc json.RawMessage, expectedVersion int) error
	// ConfigImport, if set, merges a YAML/JSON config's target branches into the DB fragment and
	// hot-reloads (POST /api/admin/config/import). Returns ErrConfigInvalid (⇒400)/ErrConfigConflict
	// (⇒409). nil ⇒ route not registered.
	ConfigImport func(body []byte) (added, unchanged, version int, err error)
}

func New(s store.Store, webDir string) *Server { return &Server{store: s, webDir: webDir} }

// activeLatest returns each active target's most recent outcome for vantage, sorted
// by name. It uses the store's bulk LatestAll (one query) when available instead of
// one Latest per target — the live endpoints (targets, charts, metrics, sla) all
// read this. A backing-store read error is returned so callers answer 503 rather
// than a false-empty success (CODE_REVIEW #4). The store.Latest fallback (used only
// when the store doesn't implement LatestAller) stays local regardless of the
// requested vantage — it exists for the minimal base Store interface, which has no
// vantage concept at all.
func (srv *Server) activeLatest(vantage string) ([]scheduler.Outcome, error) {
	var all map[string]scheduler.Outcome
	if la, ok := srv.store.(store.LatestAller); ok {
		m, err := la.LatestAll(vantage)
		if err != nil {
			return nil, err
		}
		all = m
	} else {
		keys, err := srv.store.Keys()
		if err != nil {
			return nil, err
		}
		all = map[string]scheduler.Outcome{}
		for _, k := range keys {
			if o, ok := srv.store.Latest(k); ok {
				all[k] = o
			}
		}
	}
	var active map[string]bool
	if srv.Active != nil {
		active = srv.Active()
	}
	out := make([]scheduler.Outcome, 0, len(all))
	for k, o := range all {
		if active == nil || active[k] {
			out = append(out, o)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Target.Name < out[j].Target.Name })
	return out, nil
}

func (srv *Server) Routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/probes", srv.probes)
	mux.HandleFunc("GET /api/probes/schema", srv.probeSchema)
	mux.HandleFunc("GET /api/charts", srv.charts)
	mux.HandleFunc("GET /api/sla", srv.sla)
	mux.HandleFunc("GET /api/targets", srv.targets)
	mux.HandleFunc("GET /api/series", srv.series)
	mux.HandleFunc("GET /api/series/all", srv.seriesAll)
	mux.HandleFunc("GET /api/rollup", srv.rollup)
	mux.HandleFunc("GET /metrics", srv.metrics)
	if srv.AdminPassword != "" && srv.Vantages != nil && len(srv.AdminKey) > 0 {
		mux.HandleFunc("POST /api/admin/login", srv.adminLogin)
		mux.HandleFunc("GET /api/admin/vantages", srv.requireAdmin(srv.listVantages))
		mux.HandleFunc("POST /api/admin/vantages", srv.requireAdmin(srv.addVantage))
		mux.HandleFunc("DELETE /api/admin/vantages/{name}", srv.requireAdmin(srv.revokeVantage))
	}
	if srv.AdminPassword != "" && len(srv.AdminKey) > 0 && srv.ConfigGet != nil && srv.ConfigApply != nil {
		mux.HandleFunc("GET /api/admin/config", srv.requireAdmin(srv.getConfig))
		mux.HandleFunc("PUT /api/admin/config", srv.requireAdmin(srv.putConfig))
	}
	if srv.AdminPassword != "" && len(srv.AdminKey) > 0 && srv.ConfigImport != nil {
		mux.HandleFunc("POST /api/admin/config/import", srv.requireAdmin(srv.importConfig))
	}
	if srv.VantageAuth != nil && srv.Assignment != nil {
		mux.HandleFunc("GET /agent/v1/assignment", srv.requireAgent(srv.agentAssignment))
		mux.HandleFunc("POST /agent/v1/results", srv.requireAgent(srv.agentResults))
	}
	if srv.webDir != "" {
		// Serve the SPA/static assets at the root (same-origin with the API).
		mux.Handle("GET /", http.FileServer(http.Dir(srv.webDir)))
	}
	return mux
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func fnum(v float64) *float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return nil
	}
	return &v
}

// readVantage returns the requested read vantage, defaulting to "local". An
// invalid name is a 400 (returns ok=false after writing the error).
func readVantage(w http.ResponseWriter, r *http.Request) (string, bool) {
	v := r.URL.Query().Get("vantage")
	if v == "" {
		return store.DefaultVantage, true
	}
	if !vantage.ValidName(v) {
		http.Error(w, `{"error":"invalid vantage name"}`, http.StatusBadRequest)
		return "", false
	}
	return v, true
}

func (srv *Server) probes(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{"probes": probe.Registered()})
}

// probeSchema emits each probe's config variables as JSON Schema (draft 2020-12),
// generated from the same VarSpec source that drives runtime validation — so docs
// and external validators never drift from what the collector actually accepts.
func (srv *Server) probeSchema(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{"probes": probe.AllSchemas()})
}

type targetDTO struct {
	Name     string   `json:"name"`
	Probe    string   `json:"probe"`
	MedianMs *float64 `json:"median_ms"`
	Loss     int      `json:"loss"`
	Pings    int      `json:"pings"`
	LossPct  float64  `json:"loss_pct"`
	When     string   `json:"when"`
	Error    string   `json:"error,omitempty"`
	Vantages []string `json:"vantages,omitempty"`
	// NoData marks a configured target that has no stored round for the requested
	// vantage yet — e.g. a remote-only target before its agent reports. Such a target
	// is listed (so it appears in the tree and its deep link resolves) but carries no
	// median/loss/when, and the UI shows a neutral status rather than a false "healthy".
	NoData bool `json:"no_data,omitempty"`
}

// latestDTO builds a target DTO from a stored latest round, attaching the target's
// vantage set. Shared by the live and catalog-driven listings.
func latestDTO(o scheduler.Outcome, tv map[string][]string) targetDTO {
	dto := targetDTO{
		Name:    o.Target.Name,
		Probe:   o.ProbeName,
		Loss:    o.Computed.Loss,
		Pings:   o.Computed.Pings,
		LossPct: o.Computed.LossFraction() * 100,
		When:    o.When.UTC().Format("2006-01-02T15:04:05Z"),
	}
	if m := fnum(o.Computed.Median); m != nil {
		ms := *m * 1000
		dto.MedianMs = &ms
	}
	if o.Err != nil {
		dto.Error = o.Err.Error()
	}
	if vs := tv[o.Target.Name]; len(vs) > 0 {
		dto.Vantages = vs
	}
	return dto
}

func (srv *Server) targets(w http.ResponseWriter, r *http.Request) {
	v, ok := readVantage(w, r)
	if !ok {
		return
	}
	latest, err := srv.activeLatest(v)
	if err != nil {
		slog.Error("targets: store read failed", "err", err)
		http.Error(w, `{"error":"store unavailable"}`, http.StatusServiceUnavailable)
		return
	}
	var tv map[string][]string
	if srv.TargetVantages != nil {
		tv = srv.TargetVantages()
	}
	byName := make(map[string]scheduler.Outcome, len(latest))
	for _, o := range latest {
		byName[o.Target.Name] = o
	}
	var out []targetDTO
	if srv.Configured != nil {
		// Catalog-driven: list every configured target so remote-only targets (no local
		// row) still appear and their deep links resolve; merge live data where present,
		// otherwise emit a no-data entry carrying the target's vantage set (CODE_REVIEW #3).
		for _, m := range srv.Configured() {
			if o, ok := byName[m.Name]; ok {
				out = append(out, latestDTO(o, tv))
				continue
			}
			dto := targetDTO{Name: m.Name, Probe: m.ProbeKind, NoData: true}
			if vs := tv[m.Name]; len(vs) > 0 {
				dto.Vantages = vs
			} else if len(m.Vantages) > 0 {
				dto.Vantages = m.Vantages
			}
			out = append(out, dto)
		}
	} else {
		for _, o := range latest {
			out = append(out, latestDTO(o, tv))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	writeJSON(w, map[string]any{"targets": out})
}

type chartEntry struct {
	Name     string   `json:"name"`
	Probe    string   `json:"probe"`
	MedianMs *float64 `json:"median_ms"`
	LossPct  float64  `json:"loss_pct"`
	StdDevMs *float64 `json:"stddev_ms"`
	When     string   `json:"when"`
}

// stddevMs returns the sample standard deviation of a round's received RTTs, in
// milliseconds; nil if fewer than two samples arrived (undefined jitter).
func stddevMs(sortedSec []float64) *float64 {
	if len(sortedSec) < 2 {
		return nil
	}
	var sum float64
	for _, v := range sortedSec {
		sum += v
	}
	mean := sum / float64(len(sortedSec))
	var ss float64
	for _, v := range sortedSec {
		d := v - mean
		ss += d * d
	}
	sd := math.Sqrt(ss/float64(len(sortedSec)-1)) * 1000
	return &sd
}

// charts ranks targets by their most recent round — SmokePing's "charts" (worst
// offenders). `by` selects the sort key: loss (default), median, or stddev. `n`
// caps the result (default 10). Targets with no value for the chosen key (a lost
// round has no median/stddev) are excluded from that chart, not ranked as best.
func (srv *Server) charts(w http.ResponseWriter, r *http.Request) {
	by := r.URL.Query().Get("by")
	if by == "" {
		by = "loss"
	}
	n := 10
	if v := r.URL.Query().Get("n"); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil && parsed > 0 {
			n = parsed
		}
	}
	vant, ok := readVantage(w, r)
	if !ok {
		return
	}

	type scored struct {
		e   chartEntry
		key float64
	}
	latest, err := srv.activeLatest(vant)
	if err != nil {
		slog.Error("charts: store read failed", "err", err)
		http.Error(w, `{"error":"store unavailable"}`, http.StatusServiceUnavailable)
		return
	}
	var rows []scored
	for _, o := range latest {
		e := chartEntry{
			Name:    o.Target.Name,
			Probe:   o.ProbeName,
			LossPct: o.Computed.LossFraction() * 100,
			When:    o.When.UTC().Format("2006-01-02T15:04:05Z"),
		}
		if m := fnum(o.Computed.Median); m != nil {
			ms := *m * 1000
			e.MedianMs = &ms
		}
		e.StdDevMs = stddevMs(o.Computed.Sorted)

		var key float64
		switch by {
		case "median":
			if e.MedianMs == nil {
				continue // no latency to rank a fully-lost target by
			}
			key = *e.MedianMs
		case "stddev":
			if e.StdDevMs == nil {
				continue
			}
			key = *e.StdDevMs
		case "loss":
			key = e.LossPct
		default:
			http.Error(w, `{"error":"by must be one of loss, median, stddev"}`, http.StatusBadRequest)
			return
		}
		rows = append(rows, scored{e: e, key: key})
	}

	// Worst first. Ties broken by name so the order is stable/deterministic.
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].key != rows[j].key {
			return rows[i].key > rows[j].key
		}
		return rows[i].e.Name < rows[j].e.Name
	})
	if len(rows) > n {
		rows = rows[:n]
	}
	out := make([]chartEntry, 0, len(rows))
	for _, s := range rows {
		out = append(out, s.e)
	}
	writeJSON(w, map[string]any{"by": by, "n": n, "charts": out})
}

type slaEntry struct {
	Name         string   `json:"name"`
	Probe        string   `json:"probe"`
	Measured     int      `json:"measured"`     // rounds actually measured within the window
	UpRounds     int      `json:"up_rounds"`    // rounds counted as "up"
	Availability float64  `json:"availability"` // percent: up_rounds / measured * 100
	Expected     *int     `json:"expected"`     // rounds the window should have held (window/step); null if step unknown
	CoveragePct  *float64 `json:"coverage_pct"` // min(100, measured/expected*100); null if step unknown
	AvgLossPct   float64  `json:"avg_loss_pct"` // mean loss across in-window rounds
	CoveredFrom  string   `json:"covered_from"` // oldest in-window round actually available
	Latest       string   `json:"latest"`       // timestamp of the newest in-window round
}

// slaOf reduces one target's history to an availability summary over the rounds
// at or after cutoff. isUp decides whether a round counts as available from its
// loss percentage. Pure (cutoff passed in) so it's deterministic to test.
// `oldest`/`latest` bound the rounds actually available, so a caller can tell the
// window is only partially covered (history is bounded by the store's retention).
func slaOf(hist []scheduler.Outcome, cutoff time.Time, isUp func(lossPct float64) bool) (rounds, up int, sumLoss float64, oldest, latest time.Time) {
	for _, o := range hist {
		if o.When.Before(cutoff) {
			continue
		}
		lossPct := o.Computed.LossFraction() * 100
		rounds++
		sumLoss += lossPct
		if isUp(lossPct) {
			up++
		}
		if o.When.After(latest) {
			latest = o.When
		}
		if oldest.IsZero() || o.When.Before(oldest) {
			oldest = o.When
		}
	}
	return
}

// sla reports per-target availability over a time window — the tSmoke-style
// uptime report. `window` is a Go duration (default 24h). By default a round
// counts as "up" if it got at least one reply (loss < 100%); pass `maxloss` (a
// percent) for a stricter SLA where up means loss <= maxloss. Targets with no
// rounds in the window are omitted. Sorted worst (lowest availability) first.
func (srv *Server) sla(w http.ResponseWriter, r *http.Request) {
	window := 24 * time.Hour
	if v := r.URL.Query().Get("window"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			http.Error(w, `{"error":"window must be a positive Go duration, e.g. 24h"}`, http.StatusBadRequest)
			return
		}
		window = d
	}
	// maxLossPct tightens "up" from "at least one reply" to a loss ceiling. It's
	// passed to the store aggregate; isUp mirrors it for the History fallback.
	var maxLossPct *float64
	isUp := func(lossPct float64) bool { return lossPct < 100 }
	if v := r.URL.Query().Get("maxloss"); v != "" {
		max, err := strconv.ParseFloat(v, 64)
		if err != nil || math.IsNaN(max) || math.IsInf(max, 0) || max < 0 || max > 100 {
			http.Error(w, `{"error":"maxloss must be a percent in [0,100]"}`, http.StatusBadRequest)
			return
		}
		maxLossPct = &max
		isUp = func(lossPct float64) bool { return lossPct <= max }
	}
	vant, ok := readVantage(w, r)
	if !ok {
		return
	}

	cutoff := time.Now().Add(-window)
	av, _ := srv.store.(store.Availabler)
	var steps map[string]time.Duration
	if srv.Steps != nil {
		steps = srv.Steps()
	}
	// Prefer one bulk availability scan over N per-target queries (#5 fan-out).
	var statsAll map[string]store.AvailabilityStat
	if avAll, ok := srv.store.(store.AvailabilityAller); ok {
		m, err := avAll.AvailabilityAll(r.Context(), vant, cutoff, maxLossPct)
		if err != nil {
			slog.Error("sla: bulk availability query failed", "err", err)
			http.Error(w, `{"error":"availability unavailable"}`, http.StatusServiceUnavailable)
			return
		}
		statsAll = m
	}

	// activeLatest is one bulk query for the active targets + their probe names.
	active, err := srv.activeLatest(vant)
	if err != nil {
		slog.Error("sla: store read failed", "err", err)
		http.Error(w, `{"error":"store unavailable"}`, http.StatusServiceUnavailable)
		return
	}
	out := make([]slaEntry, 0)
	for _, o := range active {
		k := o.Target.Name
		var (
			measured, up   int
			sumLoss        float64
			oldest, latest time.Time
		)
		switch {
		case statsAll != nil:
			st := statsAll[k] // zero value (measured 0) if no in-window rounds
			measured, up, sumLoss, oldest, latest = st.Measured, st.Up, st.SumLossPct, st.Oldest, st.Latest
		case av != nil:
			st, err := av.Availability(r.Context(), k, vant, cutoff, maxLossPct)
			if err != nil {
				slog.Warn("sla: availability query failed", "target", k, "err", err)
				continue
			}
			measured, up, sumLoss, oldest, latest = st.Measured, st.Up, st.SumLossPct, st.Oldest, st.Latest
		default:
			h, err := srv.store.History(k)
			if err != nil {
				slog.Warn("sla: history query failed", "target", k, "err", err)
				continue
			}
			measured, up, sumLoss, oldest, latest = slaOf(h, cutoff, isUp)
		}
		if measured == 0 {
			continue
		}
		e := slaEntry{
			Name:         k,
			Probe:        o.ProbeName,
			Measured:     measured,
			UpRounds:     up,
			Availability: float64(up) / float64(measured) * 100,
			AvgLossPct:   sumLoss / float64(measured),
			CoveredFrom:  oldest.UTC().Format("2006-01-02T15:04:05Z"),
			Latest:       latest.UTC().Format("2006-01-02T15:04:05Z"),
		}
		// Coverage: how much of the requested window we actually measured, derived
		// from the target's step. Unknown step -> availability shown without coverage.
		if step, ok := steps[k]; ok && step > 0 {
			if expected := int(window / step); expected > 0 {
				cov := float64(measured) / float64(expected) * 100
				if cov > 100 {
					cov = 100
				}
				e.Expected = &expected
				e.CoveragePct = &cov
			}
		}
		out = append(out, e)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Availability != out[j].Availability {
			return out[i].Availability < out[j].Availability // worst first
		}
		return out[i].Name < out[j].Name
	})
	writeJSON(w, map[string]any{"window": window.String(), "targets": out})
}

type roundDTO struct {
	T        string     `json:"t"`
	MedianMs *float64   `json:"median_ms"`
	Loss     int        `json:"loss"`
	Pings    int        `json:"pings"`
	RTTsMs   []*float64 `json:"rtts_ms"` // centered; null in lost slots — ready for smoke bands
}

// parseFromTo reads an optional [from,to] sub-range (unix milliseconds) for a drag-zoom
// fetch. present is true only when both are given and valid (from < to); a non-empty
// errMsg means the caller should answer 400. Absent (both empty) is the normal case.
func parseFromTo(r *http.Request) (from, to time.Time, present bool, errMsg string) {
	fs, ts := r.URL.Query().Get("from"), r.URL.Query().Get("to")
	if fs == "" && ts == "" {
		return from, to, false, ""
	}
	if fs == "" || ts == "" {
		return from, to, false, "from and to must be given together (unix milliseconds)"
	}
	fms, err1 := strconv.ParseInt(fs, 10, 64)
	tms, err2 := strconv.ParseInt(ts, 10, 64)
	if err1 != nil || err2 != nil {
		return from, to, false, "from and to must be unix-millisecond integers"
	}
	if fms >= tms {
		return from, to, false, "from must be before to"
	}
	return time.UnixMilli(fms), time.UnixMilli(tms), true, ""
}

// series returns a target's per-round smoke samples. By default it returns the
// store's recent History (bounded by the store's cap). A `window` (a Go duration,
// e.g. 30h) requests the full window instead: on a store that implements
// RangeHistorier (pgstore) it reads every round in the window — so a long raw
// range like the 30h drill-down isn't silently truncated to the last cap rounds —
// and falls back to the capped History on stores that can't (MemStore in prod is
// never that, but a bare in-memory dev store degrades honestly).
func (srv *Server) series(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get("target")
	if key == "" {
		http.Error(w, `{"error":"missing target param"}`, http.StatusBadRequest)
		return
	}
	vant, ok := readVantage(w, r)
	if !ok {
		return
	}
	// Drag-zoom sub-range: explicit [from,to] (unix ms) fetches an arbitrary historical
	// window and takes precedence over the window-based tail.
	if from, to, present, errMsg := parseFromTo(r); errMsg != "" {
		http.Error(w, `{"error":"`+errMsg+`"}`, http.StatusBadRequest)
		return
	} else if present {
		var hist []scheduler.Outcome
		if rh, ok := srv.store.(store.RangeHistorier); ok {
			h, err := rh.HistoryBetween(r.Context(), key, vant, from, to)
			if err != nil {
				slog.Error("series range query failed", "target", key, "err", err)
				http.Error(w, `{"error":"series unavailable"}`, http.StatusServiceUnavailable)
				return
			}
			hist = h
		} else {
			h, err := srv.store.History(key) // best-effort: capped, but honest
			if err != nil {
				slog.Error("series query failed", "target", key, "err", err)
				http.Error(w, `{"error":"series unavailable"}`, http.StatusServiceUnavailable)
				return
			}
			hist = h
		}
		writeJSON(w, map[string]any{"target": key, "rounds": roundsDTO(hist)})
		return
	}
	var hist []scheduler.Outcome
	if v := r.URL.Query().Get("window"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			http.Error(w, `{"error":"window must be a positive Go duration, e.g. 30h"}`, http.StatusBadRequest)
			return
		}
		if rh, ok := srv.store.(store.RangeHistorier); ok {
			h, err := rh.HistorySince(r.Context(), key, vant, time.Now().Add(-d))
			if err != nil {
				slog.Error("series window query failed", "target", key, "err", err)
				http.Error(w, `{"error":"series unavailable"}`, http.StatusServiceUnavailable)
				return
			}
			hist = h
		} else {
			h, err := srv.store.History(key) // best-effort: capped, but honest
			if err != nil {
				slog.Error("series query failed", "target", key, "err", err)
				http.Error(w, `{"error":"series unavailable"}`, http.StatusServiceUnavailable)
				return
			}
			hist = h
		}
	} else {
		// No window: the recent capped tail for the SELECTED vantage. Use the vantage-aware
		// read when the store supports it so ?vantage=nyc returns nyc's rounds, not the
		// local-only History (CODE_REVIEW #7 / P2-7); a bare store degrades to local History.
		var h []scheduler.Outcome
		var err error
		if rv, ok := srv.store.(store.RecentHistorier); ok {
			h, err = rv.HistoryVantage(r.Context(), key, vant)
		} else {
			h, err = srv.store.History(key)
		}
		if err != nil {
			slog.Error("series query failed", "target", key, "err", err)
			http.Error(w, `{"error":"series unavailable"}`, http.StatusServiceUnavailable)
			return
		}
		hist = h
	}
	writeJSON(w, map[string]any{"target": key, "rounds": roundsDTO(hist)})
}

// roundsDTO converts a target's rounds (oldest->newest) into the wire shape shared by
// /api/series and /api/series/all: each round carries its wall-clock timestamp `t`,
// loss/pings, and the centered RTTs (null in lost slots) ready for the smoke bands.
func roundsDTO(hist []scheduler.Outcome) []roundDTO {
	rounds := make([]roundDTO, 0, len(hist))
	for _, o := range hist {
		rd := roundDTO{
			T:     o.When.UTC().Format("2006-01-02T15:04:05.000Z"), // ms precision: the grid cursor is this string parsed to ms; whole seconds let two rounds in one second collide and drop one (#3)
			Loss:  o.Computed.Loss,
			Pings: o.Computed.Pings,
		}
		if m := fnum(o.Computed.Median); m != nil {
			ms := *m * 1000
			rd.MedianMs = &ms
		}
		for _, v := range o.Computed.Centered {
			if n := fnum(v); n != nil {
				ms := *n * 1000
				rd.RTTsMs = append(rd.RTTsMs, &ms)
			} else {
				rd.RTTsMs = append(rd.RTTsMs, nil)
			}
		}
		rounds = append(rounds, rd)
	}
	return rounds
}

// maxGridWindow bounds the bulk grid endpoint's lookback. The Graphs grid only ever
// asks for the recent window (3h); this is generous headroom while preventing an
// all-targets full-table scan from a hand-crafted request. Long ranges use /api/rollup.
const maxGridWindow = 48 * time.Hour

// maxSeriesAllTotalRounds bounds the TOTAL rounds /api/series/all serializes across all targets.
// SeriesAll caps rows per target (20k), but a bulk request over many targets could still
// materialize a huge response (1000 targets × 20k = 20M) — an authenticated memory/DoS sink on the
// response path. capSeriesAll trims to this budget, keeping the newest rounds of each target so
// every target stays represented, and the handler surfaces `truncated` (CODE_REVIEW M5).
const maxSeriesAllTotalRounds = 300_000

// capSeriesAll trims all[*] in place so the total rounds across every target does not exceed max,
// keeping each target's NEWEST rounds (an equal per-target share) so every target stays
// represented. Returns whether anything was trimmed. Each target's rounds are oldest→newest.
func capSeriesAll(all map[string][]scheduler.Outcome, max int) bool {
	total := 0
	for _, h := range all {
		total += len(h)
	}
	if total <= max || len(all) == 0 {
		return false
	}
	share := max / len(all)
	if share < 1 {
		share = 1 // more targets than the budget: keep each target's single newest round
	}
	trimmed := false
	for name, h := range all {
		if len(h) > share {
			all[name] = h[len(h)-share:]
			trimmed = true
		}
	}
	return trimmed
}

// seriesAll returns recent per-round samples for every target in one response — the
// bulk, incremental read behind the Graphs grid. `window` (required, a Go duration)
// bounds the first fetch; `since` (unix ms) is the client's watermark, so a refresh
// transfers only rounds newer than it instead of the whole window every tick. The
// effective cutoff is max(now-window, since). One store query serves all targets,
// replacing the previous one-request-and-query-per-target fan-out (CODE_REVIEW #2).
func (srv *Server) seriesAll(w http.ResponseWriter, r *http.Request) {
	v := r.URL.Query().Get("window")
	if v == "" {
		http.Error(w, `{"error":"window is required, e.g. 3h"}`, http.StatusBadRequest)
		return
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		http.Error(w, `{"error":"window must be a positive Go duration, e.g. 3h"}`, http.StatusBadRequest)
		return
	}
	if d > maxGridWindow {
		http.Error(w, `{"error":"window too large for the bulk grid endpoint; use /api/rollup for long ranges"}`, http.StatusBadRequest)
		return
	}
	sa, ok := srv.store.(store.SeriesAller)
	if !ok {
		http.Error(w, `{"error":"bulk series not supported by this store"}`, http.StatusNotImplemented)
		return
	}
	vant, ok := readVantage(w, r)
	if !ok {
		return
	}
	// cutoff = max(now-window, since): never scan older than the window, and on an
	// incremental refresh the watermark dominates so only new rounds come back.
	cutoff := time.Now().Add(-d)
	if sv := r.URL.Query().Get("since"); sv != "" {
		ms, err := strconv.ParseInt(sv, 10, 64)
		if err != nil {
			http.Error(w, `{"error":"since must be a unix-millisecond integer"}`, http.StatusBadRequest)
			return
		}
		if st := time.UnixMilli(ms); st.After(cutoff) {
			cutoff = st
		}
	}
	all, err := sa.SeriesAll(r.Context(), vant, cutoff)
	if err != nil {
		slog.Error("series/all query failed", "err", err)
		http.Error(w, `{"error":"series unavailable"}`, http.StatusServiceUnavailable)
		return
	}
	// Bound the total response across all targets, keeping each target's newest rounds. Trimming is
	// visible via `truncated` so a client can narrow the window / use /api/rollup (CODE_REVIEW M5).
	truncated := capSeriesAll(all, maxSeriesAllTotalRounds)
	targets := make(map[string]any, len(all))
	total := 0
	for name, hist := range all {
		total += len(hist)
		targets[name] = map[string]any{"rounds": roundsDTO(hist)}
	}
	if truncated {
		slog.Warn("series/all response truncated to the global round budget",
			"budget", maxSeriesAllTotalRounds, "targets", len(all), "vantage", vant)
	}
	writeJSON(w, map[string]any{
		"cutoff":    cutoff.UTC().Format("2006-01-02T15:04:05Z"),
		"targets":   targets,
		"rounds":    total,
		"truncated": truncated,
	})
}

type rollupDTO struct {
	Bucket      string   `json:"bucket"`
	MedianAvgMs *float64 `json:"median_avg_ms"`
	MedianMinMs *float64 `json:"median_min_ms"`
	MedianMaxMs *float64 `json:"median_max_ms"`
	LossPct     float64  `json:"loss_pct"`
	Rounds      int      `json:"rounds"`
	// MedianRounds is the rounds that produced a median (not fully lost) — the weight the
	// client uses for median statistics, distinct from Rounds (loss weight) (P2-6).
	MedianRounds int `json:"median_rounds"`
}

func msPtr(seconds float64) *float64 {
	if math.IsNaN(seconds) || math.IsInf(seconds, 0) {
		return nil
	}
	ms := seconds * 1000
	return &ms
}

// rollup returns downsampled buckets for a target (the coarse tiers a long-range
// view reads). `res` picks the tier: "1h" (default) or "1d" — daily feeds the
// 400d range, where hourly would be too many buckets. Requires a store that
// implements Rollupper (pgstore).
func (srv *Server) rollup(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get("target")
	if key == "" {
		http.Error(w, `{"error":"missing target param"}`, http.StatusBadRequest)
		return
	}
	res := r.URL.Query().Get("res")
	if res == "" {
		res = "1h"
	}
	if res != "1h" && res != "1d" {
		http.Error(w, `{"error":"res must be 1h or 1d"}`, http.StatusBadRequest)
		return
	}
	vant, ok := readVantage(w, r)
	if !ok {
		return
	}
	// Bounds: an explicit [from,to] (unix ms) drag-zoom sub-range takes precedence;
	// otherwise an optional window (a Go duration, e.g. 240h) bounds to the recent tail
	// so a long-range view doesn't transfer the whole retained history.
	var since, until time.Time
	if from, to, present, errMsg := parseFromTo(r); errMsg != "" {
		http.Error(w, `{"error":"`+errMsg+`"}`, http.StatusBadRequest)
		return
	} else if present {
		since, until = from, to
	} else if v := r.URL.Query().Get("window"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			http.Error(w, `{"error":"window must be a positive Go duration, e.g. 240h"}`, http.StatusBadRequest)
			return
		}
		since = time.Now().Add(-d)
	}
	rp, ok := srv.store.(store.Rollupper)
	if !ok {
		http.Error(w, `{"error":"rollup requires the TimescaleDB store (run with -dsn -downsample)"}`, http.StatusNotImplemented)
		return
	}
	points, err := rp.Rollup(r.Context(), key, vant, res, since, until)
	if err != nil {
		// A store that implements Rollupper but whose hourly aggregate was never
		// created (a Compose DB started without -downsample) should look "hourly not
		// supported", not "temporarily broken" — 501 tells the UI to disable hourly
		// mode instead of leaving it stuck on failing panels.
		if errors.Is(err, store.ErrRollupUnavailable) {
			http.Error(w, `{"error":"rollup not enabled (run with -dsn -downsample)"}`, http.StatusNotImplemented)
			return
		}
		// Log the real cause server-side; return a generic message so internal
		// detail (table names, driver internals) never reaches the client.
		slog.Error("rollup query failed", "target", key, "err", err)
		http.Error(w, `{"error":"rollup unavailable"}`, http.StatusServiceUnavailable)
		return
	}
	buckets := make([]rollupDTO, 0, len(points))
	for _, p := range points {
		buckets = append(buckets, rollupDTO{
			Bucket:       p.Bucket.UTC().Format("2006-01-02T15:04:05Z"),
			MedianAvgMs:  msPtr(p.MedianAvg),
			MedianMinMs:  msPtr(p.MedianMin),
			MedianMaxMs:  msPtr(p.MedianMax),
			LossPct:      p.LossFrac * 100,
			Rounds:       p.Rounds,
			MedianRounds: p.MedianRounds,
		})
	}
	writeJSON(w, map[string]any{"target": key, "resolution": res, "buckets": buckets})
}

// metrics exposes the latest per-target values in Prometheus text format so
// Grafana/Alertmanager-native setups can scrape and alert on them.
func (srv *Server) metrics(w http.ResponseWriter, _ *http.Request) {
	// Read the store first: a failure must be a 503 so the scrape is marked down,
	// not a 200 that silently drops every target series (CODE_REVIEW #4). Always the
	// hub's own local vantage — a Prometheus scrape is this hub's own view, not a
	// remote agent's (no ?vantage= param on /metrics).
	latest, err := srv.activeLatest(store.DefaultVantage)
	if err != nil {
		slog.Error("metrics: store read failed", "err", err)
		http.Error(w, "store unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	var b strings.Builder
	b.WriteString("# HELP smokeping_probe_median_seconds Median round-trip time of the most recent round.\n")
	b.WriteString("# TYPE smokeping_probe_median_seconds gauge\n")
	b.WriteString("# HELP smokeping_probe_loss_ratio Fraction of pings lost in the most recent round (0..1).\n")
	b.WriteString("# TYPE smokeping_probe_loss_ratio gauge\n")
	b.WriteString("# HELP smokeping_probe_up 1 if the most recent round got at least one reply, else 0.\n")
	b.WriteString("# TYPE smokeping_probe_up gauge\n")
	b.WriteString("# HELP smokeping_probe_duration_seconds Wall-clock the most recent measurement of this target took.\n")
	b.WriteString("# TYPE smokeping_probe_duration_seconds gauge\n")
	b.WriteString("# HELP smokeping_probe_last_sample_timestamp_seconds Unix time of this target's most recent round (alert on staleness).\n")
	b.WriteString("# TYPE smokeping_probe_last_sample_timestamp_seconds gauge\n")
	for _, o := range latest {
		lbl := fmt.Sprintf(`{target=%q,probe=%q}`, escapeLabel(o.Target.Name), escapeLabel(o.ProbeName))
		median := o.Computed.Median
		if math.IsNaN(median) {
			fmt.Fprintf(&b, "smokeping_probe_median_seconds%s NaN\n", lbl)
		} else {
			fmt.Fprintf(&b, "smokeping_probe_median_seconds%s %g\n", lbl, median)
		}
		fmt.Fprintf(&b, "smokeping_probe_loss_ratio%s %g\n", lbl, o.Computed.LossFraction())
		up := 0
		if o.Computed.Loss < o.Computed.Pings {
			up = 1
		}
		fmt.Fprintf(&b, "smokeping_probe_up%s %d\n", lbl, up)
		fmt.Fprintf(&b, "smokeping_probe_duration_seconds%s %g\n", lbl, o.Duration.Seconds())
		fmt.Fprintf(&b, "smokeping_probe_last_sample_timestamp_seconds%s %d\n", lbl, o.When.Unix())
	}
	srv.writeRoundMetrics(&b)
	srv.writeIngestMetrics(&b)
	if srv.ExtraMetrics != nil {
		srv.ExtraMetrics(&b)
	}
	_, _ = w.Write([]byte(b.String()))
}

// recordMissingFingerprint counts n rounds from `vantage` that carried no measurement
// fingerprint (a pre-fingerprint agent) and warns once per vantage. The count is scrapeable
// (smokeping_agent_missing_fingerprint_total) so an operator can tell when every vantage's agent
// has been upgraded; the warning's wording reflects whether those rounds were dropped (strict
// mode) or accepted-unverified (lenient default) (CODE_REVIEW #2).
func (srv *Server) recordMissingFingerprint(vantage string, n int) {
	srv.ingestMu.Lock()
	if srv.missingFP == nil {
		srv.missingFP = map[string]int64{}
	}
	if srv.warnedMissingFP == nil {
		srv.warnedMissingFP = map[string]bool{}
	}
	first := !srv.warnedMissingFP[vantage]
	srv.warnedMissingFP[vantage] = true
	srv.missingFP[vantage] += int64(n)
	srv.ingestMu.Unlock()
	if first {
		if srv.RequireFingerprint {
			slog.Warn("agent ingest: DROPPING rounds with no fingerprint (strict mode enabled); upgrade the agent to a fingerprinting build", "vantage", vantage)
		} else {
			slog.Warn("agent ingest: accepting rounds with no fingerprint; result attribution is unverified until the agent is upgraded", "vantage", vantage)
		}
	}
}

// writeIngestMetrics renders the per-vantage agent-ingest observability counters.
func (srv *Server) writeIngestMetrics(b *strings.Builder) {
	srv.ingestMu.Lock()
	defer srv.ingestMu.Unlock()
	if len(srv.missingFP) == 0 {
		return
	}
	vantages := make([]string, 0, len(srv.missingFP))
	for v := range srv.missingFP {
		vantages = append(vantages, v)
	}
	sort.Strings(vantages) // deterministic scrape output
	b.WriteString("# HELP smokeping_agent_missing_fingerprint_total Agent rounds received with no measurement fingerprint (a pre-fingerprint agent): accepted-unverified in the lenient default, or dropped in strict mode (-require-fingerprint).\n")
	b.WriteString("# TYPE smokeping_agent_missing_fingerprint_total counter\n")
	for _, v := range vantages {
		fmt.Fprintf(b, "smokeping_agent_missing_fingerprint_total{vantage=%q} %d\n", escapeLabel(v), srv.missingFP[v])
	}
}

// writeRoundMetrics appends collector round-level operational metrics, if the
// server was given a RoundStats and at least one round has completed.
func (srv *Server) writeRoundMetrics(b *strings.Builder) {
	rs, ok := srv.Rounds.snapshot()
	if !ok {
		return
	}
	b.WriteString("# HELP smokeping_rounds_total Measurement rounds completed since start.\n")
	b.WriteString("# TYPE smokeping_rounds_total counter\n")
	fmt.Fprintf(b, "smokeping_rounds_total %d\n", rs.total)
	b.WriteString("# HELP smokeping_round_duration_seconds Wall-clock of the most recent round.\n")
	b.WriteString("# TYPE smokeping_round_duration_seconds gauge\n")
	fmt.Fprintf(b, "smokeping_round_duration_seconds %g\n", rs.duration.Seconds())
	b.WriteString("# HELP smokeping_round_targets Targets measured in the most recent round.\n")
	b.WriteString("# TYPE smokeping_round_targets gauge\n")
	fmt.Fprintf(b, "smokeping_round_targets %d\n", rs.targets)
	b.WriteString("# HELP smokeping_round_errors Targets that errored in the most recent round.\n")
	b.WriteString("# TYPE smokeping_round_errors gauge\n")
	fmt.Fprintf(b, "smokeping_round_errors %d\n", rs.errs)
	b.WriteString("# HELP smokeping_last_round_timestamp_seconds Start time of the most recent round.\n")
	b.WriteString("# TYPE smokeping_last_round_timestamp_seconds gauge\n")
	fmt.Fprintf(b, "smokeping_last_round_timestamp_seconds %d\n", rs.lastUnix)
}

// escapeLabel escapes a Prometheus label value (backslash, double-quote, newline).
func escapeLabel(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	return s
}

const adminCookie = "smoked_admin"

func (srv *Server) adminLogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		http.Error(w, `{"error":"bad request"}`, http.StatusBadRequest)
		return
	}
	if subtle.ConstantTimeCompare([]byte(body.Password), []byte(srv.AdminPassword)) != 1 {
		http.Error(w, `{"error":"invalid password"}`, http.StatusUnauthorized)
		return
	}
	w.Header().Set("Cache-Control", "no-store") // never cache a session-issuing response
	http.SetCookie(w, &http.Cookie{
		Name: adminCookie, Value: signSession(srv.AdminKey, time.Now().Add(12*time.Hour)),
		Path: "/api/admin", HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode, MaxAge: 12 * 3600,
	})
	w.WriteHeader(http.StatusNoContent)
}

func (srv *Server) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(adminCookie)
		if err != nil || !verifySession(srv.AdminKey, c.Value, time.Now()) {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func (srv *Server) listVantages(w http.ResponseWriter, r *http.Request) {
	infos, err := srv.Vantages.List(r.Context())
	if err != nil {
		http.Error(w, `{"error":"store unavailable"}`, http.StatusServiceUnavailable)
		return
	}
	// counts[v] = number of configured targets whose vantage set contains v. Nil map
	// (no TargetVantages wired) means the count is unknown, not zero — omit the field.
	var counts map[string]int
	if srv.TargetVantages != nil {
		counts = map[string]int{}
		for _, vs := range srv.TargetVantages() {
			seen := map[string]bool{}
			for _, v := range vs {
				if seen[v] {
					continue
				}
				seen[v] = true
				counts[v]++
			}
		}
	}
	out := make([]map[string]any, 0, len(infos))
	for _, in := range infos {
		var last any
		if !in.LastSeen.IsZero() {
			last = in.LastSeen.UTC().Format(time.RFC3339)
		}
		row := map[string]any{"name": in.Name, "created": in.Created.UTC().Format(time.RFC3339), "last_seen": last}
		if counts != nil {
			row["targets"] = counts[in.Name]
		}
		out = append(out, row)
	}
	writeJSON(w, map[string]any{"vantages": out})
}

func (srv *Server) addVantage(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil || body.Name == "" {
		http.Error(w, `{"error":"name required"}`, http.StatusBadRequest)
		return
	}
	if !vantage.ValidName(body.Name) {
		http.Error(w, `{"error":"invalid vantage name (use letters, digits, . _ -)"}`, http.StatusBadRequest)
		return
	}
	key, err := srv.Vantages.Add(r.Context(), body.Name)
	if err != nil {
		switch {
		case errors.Is(err, vantage.ErrReserved):
			http.Error(w, `{"error":"\"local\" is reserved for the hub"}`, http.StatusConflict)
		case errors.Is(err, vantage.ErrInvalidName):
			http.Error(w, `{"error":"invalid vantage name (use letters, digits, . _ -)"}`, http.StatusBadRequest)
		default:
			slog.Error("addVantage: store add failed", "err", err)
			http.Error(w, `{"error":"store unavailable"}`, http.StatusServiceUnavailable)
		}
		return
	}
	w.Header().Set("Cache-Control", "no-store") // the one-time key is in this body — never cache it
	writeJSON(w, map[string]any{"name": body.Name, "key": key, "snippet": vantage.AgentSnippet(body.Name, key)})
}

func (srv *Server) revokeVantage(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	removed, err := srv.Vantages.Revoke(r.Context(), name)
	if err != nil {
		http.Error(w, `{"error":"store unavailable"}`, http.StatusServiceUnavailable)
		return
	}
	if !removed {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	writeJSON(w, map[string]any{"removed": true, "name": name})
}

func (srv *Server) getConfig(w http.ResponseWriter, r *http.Request) {
	doc, version, err := srv.ConfigGet()
	if err != nil {
		http.Error(w, `{"error":"store unavailable"}`, http.StatusServiceUnavailable)
		return
	}
	if trimmed := bytes.TrimSpace(doc); len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		doc = json.RawMessage(`{}`)
	}
	writeJSON(w, map[string]any{"version": version, "doc": doc})
}

func (srv *Server) putConfig(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Version int             `json:"version"`
		Doc     json.RawMessage `json:"doc"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		http.Error(w, `{"error":"bad request"}`, http.StatusBadRequest)
		return
	}
	trimmed := bytes.TrimSpace(body.Doc)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		http.Error(w, `{"error":"doc required"}`, http.StatusBadRequest)
		return
	}
	err := srv.ConfigApply(body.Doc, body.Version)
	switch {
	case err == nil:
		writeJSON(w, map[string]any{"version": body.Version + 1})
	case errors.Is(err, ErrConfigInvalid):
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
	case errors.Is(err, ErrConfigConflict):
		http.Error(w, `{"error":"version conflict"}`, http.StatusConflict)
	default:
		http.Error(w, `{"error":"apply failed"}`, http.StatusInternalServerError)
	}
}

func (srv *Server) importConfig(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		http.Error(w, `{"error":"bad request"}`, http.StatusBadRequest)
		return
	}
	added, unchanged, version, err := srv.ConfigImport(body)
	switch {
	case err == nil:
		writeJSON(w, map[string]any{"added": added, "unchanged": unchanged, "version": version})
	case errors.Is(err, ErrConfigInvalid):
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
	case errors.Is(err, ErrConfigConflict):
		http.Error(w, `{"error":"version conflict"}`, http.StatusConflict)
	default:
		http.Error(w, `{"error":"import failed"}`, http.StatusInternalServerError)
	}
}
