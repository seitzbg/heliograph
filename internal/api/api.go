// Package api exposes the collected series as JSON — the modern replacement for
// SmokePing's CGI (see codemap 05 §7). The frontend SPA would render the smoke
// graph client-side from /api/series. NaN medians serialize as null.
package api

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"smokeping-modern/internal/probe"
	"smokeping-modern/internal/scheduler"
	"smokeping-modern/internal/store"
)

type Server struct {
	store  store.Store
	webDir string
	// Rounds, if set, adds collector round-level metrics (duration, size, error
	// count) to /metrics. Optional; nil in tests and pure-API use.
	Rounds *RoundStats
}

func New(s store.Store, webDir string) *Server { return &Server{store: s, webDir: webDir} }

func (srv *Server) Routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/probes", srv.probes)
	mux.HandleFunc("GET /api/probes/schema", srv.probeSchema)
	mux.HandleFunc("GET /api/charts", srv.charts)
	mux.HandleFunc("GET /api/sla", srv.sla)
	mux.HandleFunc("GET /api/targets", srv.targets)
	mux.HandleFunc("GET /api/series", srv.series)
	mux.HandleFunc("GET /api/rollup", srv.rollup)
	mux.HandleFunc("GET /metrics", srv.metrics)
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
}

func (srv *Server) targets(w http.ResponseWriter, _ *http.Request) {
	var out []targetDTO
	for _, k := range srv.store.Keys() {
		o, ok := srv.store.Latest(k)
		if !ok {
			continue
		}
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
		out = append(out, dto)
	}
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

	type scored struct {
		e   chartEntry
		key float64
	}
	var rows []scored
	for _, k := range srv.store.Keys() {
		o, ok := srv.store.Latest(k)
		if !ok {
			continue
		}
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
	Name         string  `json:"name"`
	Probe        string  `json:"probe"`
	Rounds       int     `json:"rounds"`       // measured rounds within the window
	UpRounds     int     `json:"up_rounds"`    // rounds counted as "up"
	Availability float64 `json:"availability"` // percent: up_rounds / rounds * 100
	AvgLossPct   float64 `json:"avg_loss_pct"` // mean loss across in-window rounds
	CoveredFrom  string  `json:"covered_from"` // oldest in-window round actually available
	Latest       string  `json:"latest"`       // timestamp of the newest in-window round
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
	isUp := func(lossPct float64) bool { return lossPct < 100 }
	if v := r.URL.Query().Get("maxloss"); v != "" {
		max, err := strconv.ParseFloat(v, 64)
		if err != nil || max < 0 {
			http.Error(w, `{"error":"maxloss must be a non-negative percent"}`, http.StatusBadRequest)
			return
		}
		isUp = func(lossPct float64) bool { return lossPct <= max }
	}

	cutoff := time.Now().Add(-window)
	out := make([]slaEntry, 0)
	for _, k := range srv.store.Keys() {
		rounds, up, sumLoss, oldest, latest := slaOf(srv.store.History(k), cutoff, isUp)
		if rounds == 0 {
			continue
		}
		o, _ := srv.store.Latest(k)
		out = append(out, slaEntry{
			Name:         k,
			Probe:        o.ProbeName,
			Rounds:       rounds,
			UpRounds:     up,
			Availability: float64(up) / float64(rounds) * 100,
			AvgLossPct:   sumLoss / float64(rounds),
			CoveredFrom:  oldest.UTC().Format("2006-01-02T15:04:05Z"),
			Latest:       latest.UTC().Format("2006-01-02T15:04:05Z"),
		})
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

func (srv *Server) series(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get("target")
	if key == "" {
		http.Error(w, `{"error":"missing target param"}`, http.StatusBadRequest)
		return
	}
	hist := srv.store.History(key)
	rounds := make([]roundDTO, 0, len(hist))
	for _, o := range hist {
		rd := roundDTO{
			T:     o.When.UTC().Format("2006-01-02T15:04:05Z"),
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
	writeJSON(w, map[string]any{"target": key, "rounds": rounds})
}

type rollupDTO struct {
	Bucket      string   `json:"bucket"`
	MedianAvgMs *float64 `json:"median_avg_ms"`
	MedianMinMs *float64 `json:"median_min_ms"`
	MedianMaxMs *float64 `json:"median_max_ms"`
	LossPct     float64  `json:"loss_pct"`
	Rounds      int      `json:"rounds"`
}

func msPtr(seconds float64) *float64 {
	if math.IsNaN(seconds) || math.IsInf(seconds, 0) {
		return nil
	}
	ms := seconds * 1000
	return &ms
}

// rollup returns the hourly downsampled buckets for a target (the coarse tier a
// long-range view reads). Requires a store that implements Rollupper (pgstore).
func (srv *Server) rollup(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get("target")
	if key == "" {
		http.Error(w, `{"error":"missing target param"}`, http.StatusBadRequest)
		return
	}
	rp, ok := srv.store.(store.Rollupper)
	if !ok {
		http.Error(w, `{"error":"rollup requires the TimescaleDB store (run with -dsn -downsample)"}`, http.StatusNotImplemented)
		return
	}
	points, err := rp.Rollup(r.Context(), key)
	if err != nil {
		// Log the real cause server-side; return a generic message so internal
		// detail (table names, driver internals) never reaches the client.
		slog.Error("rollup query failed", "target", key, "err", err)
		http.Error(w, `{"error":"rollup unavailable"}`, http.StatusServiceUnavailable)
		return
	}
	buckets := make([]rollupDTO, 0, len(points))
	for _, p := range points {
		buckets = append(buckets, rollupDTO{
			Bucket:      p.Bucket.UTC().Format("2006-01-02T15:04:05Z"),
			MedianAvgMs: msPtr(p.MedianAvg),
			MedianMinMs: msPtr(p.MedianMin),
			MedianMaxMs: msPtr(p.MedianMax),
			LossPct:     p.LossFrac * 100,
			Rounds:      p.Rounds,
		})
	}
	writeJSON(w, map[string]any{"target": key, "resolution": "1h", "buckets": buckets})
}

// metrics exposes the latest per-target values in Prometheus text format so
// Grafana/Alertmanager-native setups can scrape and alert on them.
func (srv *Server) metrics(w http.ResponseWriter, _ *http.Request) {
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
	for _, k := range srv.store.Keys() {
		o, ok := srv.store.Latest(k)
		if !ok {
			continue
		}
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
	}
	srv.writeRoundMetrics(&b)
	_, _ = w.Write([]byte(b.String()))
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
