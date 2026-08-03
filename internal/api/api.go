// Package api exposes the collected series as JSON — the modern replacement for
// SmokePing's CGI (see codemap 05 §7). The frontend SPA would render the smoke
// graph client-side from /api/series. NaN medians serialize as null.
package api

import (
	"encoding/json"
	"math"
	"net/http"

	"smokeping-modern/internal/probe"
	"smokeping-modern/internal/store"
)

type Server struct {
	store  *store.Store
	webDir string
}

func New(s *store.Store, webDir string) *Server { return &Server{store: s, webDir: webDir} }

func (srv *Server) Routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/probes", srv.probes)
	mux.HandleFunc("GET /api/targets", srv.targets)
	mux.HandleFunc("GET /api/series", srv.series)
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
