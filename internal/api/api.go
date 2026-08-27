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

	"github.com/seitzbg/heliograph/internal/model"
	"github.com/seitzbg/heliograph/internal/probe"
	"github.com/seitzbg/heliograph/internal/probe/ntpprobe"
	"github.com/seitzbg/heliograph/internal/scheduler"
	"github.com/seitzbg/heliograph/internal/store"
	"github.com/seitzbg/heliograph/internal/vantage"
)

// VantageAdmin is the subset of the vantage registry the admin API uses. Kept an
// interface so the handlers test with a fake and api needs no live DB.
type VantageAdmin interface {
	Register(ctx context.Context, name string) error
	List(ctx context.Context) ([]vantage.Info, error)
	Revoke(ctx context.Context, name string) (removed bool, err error)
	// IsActive reports whether name is a known, non-revoked vantage — the authorization check
	// requireAgent runs against a client cert's CommonName (mTLS federation auth).
	IsActive(ctx context.Context, name string) (bool, error)
	// IssueClientCert mints a fresh mTLS client identity (leaf cert + key, each PEM-encoded) for
	// name from the hub's federation CA, plus the CA's own cert PEM so the caller can verify the
	// hub in turn. addVantage calls this immediately after Register so the one-click "add
	// vantage" admin flow can hand back a ready-to-run onboarding bundle.
	IssueClientCert(ctx context.Context, name string) (certPEM, keyPEM, caPEM []byte, err error)
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
	// Version is the collector's build version (git-describe: latest tag + commits + short SHA,
	// or "dev" for an unversioned build). Surfaced at GET /api/version for the dashboard footer.
	Version string
	// AbsoluteTime controls whether the dashboard labels graph x-axes with absolute clock time
	// (true, the default) or relative -3h/now labels. Surfaced at GET /api/version; set from
	// -absolute-time / SMOKED_ABSOLUTE_TIME. Applies uniformly to every graph (grid, stack, zoom).
	AbsoluteTime bool
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
	// AdminPassword + AdminKey enable login/logout/session. AdminKey must be independent,
	// high-entropy signing material (production loads SMOKED_ADMIN_SESSION_KEY, or generates a
	// process-local fallback). Vantages additionally enables its key-management routes; ConfigGet /
	// ConfigApply separately enable config CRUD. An empty password or key registers no admin routes.
	Vantages      VantageAdmin
	AdminPassword string
	AdminKey      []byte
	// AgentHubURL is the hub's own externally-reachable base URL (scheme + host + mTLS port,
	// e.g. "https://heliograph.bsd-unix.net:8443"), embedded into a minted vantage's agent.yaml
	// so the agent knows where to report. Populated in main from the first -agent-hostname and the
	// port of -agent-addr (so a non-default -agent-addr port is reflected); empty here means no flag
	// was set, and addVantage falls back to a placeholder.
	AgentHubURL string
	// AdminSessionTTL is how long a login stays valid (token expiry + cookie Max-Age). Zero or
	// negative means DefaultAdminSessionTTL; production loads SMOKED_ADMIN_SESSION_TTL.
	AdminSessionTTL time.Duration
	// TargetVantages, if set, returns each configured target's assigned vantage set (name
	// -> vantage names) — /api/targets surfaces it so the UI can show which vantages a
	// target is measured from. nil means the field is omitted (tests/pure API/no federation).
	TargetVantages func() map[string][]string
	// TargetMeta, if set, returns each target's display-only metadata (title override + the
	// IP to show in the graph title). nil means the field is omitted.
	TargetMeta func() map[string]TargetMeta
	// NTPStat, if set, returns the most recent clock offset (seconds), stratum, and graphed metric
	// ("rtt"/"offset") measured for an NTP target (ok=false for non-NTP targets or before the first
	// measurement). The caller passes the target's CURRENT measurement identity (ntpprobe.StatKey:
	// host+port+version) as wantKey; a stored reading whose key no longer matches is refused
	// (ok=false), so after a target is repointed at a different NTP server/port/version the old
	// endpoint's offset/stratum is never shown for the new one (CODE_REVIEW M3). /api/targets surfaces
	// it so the dashboard shows offset + stratum as stats (and, on an offset-graphing panel, hides the
	// now-redundant offset stat). nil = the fields are omitted (no NTP probe registered / pure API
	// tests). Wired to ntpprobe.LatestFor in main. NTPStat is the HUB's own (local-vantage) reading;
	// a remote vantage's reading comes from remoteNTP instead.
	NTPStat func(target, wantKey string) (offsetSec float64, stratum uint8, measure string, ok bool)
	// remoteNTP holds each remote vantage's latest NTP clock stat, fed by agent RoundReports on
	// ingest and read by /api/targets for a non-local vantage. Keyed by (vantage, target) so a
	// remote reading is never attributed to the hub or another vantage (CODE_REVIEW M2/M3).
	remoteNTP remoteNTPStats
	// Configured, if set, returns the full configured target catalog (all vantages), so
	// /api/targets lists a target even when it has no stored row for the requested vantage
	// yet — e.g. a remote-only target the hub never probes locally (CODE_REVIEW #3 / P1-3).
	// Without it, /api/targets shows only targets that already have a latest row, hiding
	// remote-only targets from the tree and their deep links. It is also the required bounded
	// target catalog for /api/series/all; that endpoint returns 503 when Configured is nil rather
	// than falling back to raw-history target discovery.
	Configured func() []model.Monitor
	// EffectiveMetric, if set, returns a target's metric ("rtt"/"offset") resolved from the CURRENT
	// runtime config — the probe-level `measure` default plus any per-target override. It is the
	// authoritative source for the signed-axis choice and for filtering a target's series/rollup to
	// one metric, so a config change takes effect immediately and a probe-level default is honored
	// even before the target has any stored round. nil falls back to the stored round's kind.
	EffectiveMetric func(target string) string
	// Assignment, if set, returns the target list, the effective probe-level config
	// (probe kind -> its `probes.<Kind>` block), and a config_version for a vantage,
	// computed over the live monitor set. Used by agentAssignment/agentResults, which the mTLS
	// agent listener serves via AgentMux (wired in cmd/smoked when -agent-addr is set) behind
	// requireAgent — that authorizes a caller by client-cert CN via srv.Vantages.IsActive. These
	// routes are deliberately NOT on the public Routes() mux.
	Assignment func(vantage string) (targets []model.Monitor, probeCfgs map[string]map[string]string, configVersion string)
	// OnIngest, if set, is called with the accepted remote outcomes AFTER they are durably
	// stored, so the hub evaluates alerts for remote vantages (each outcome carries its
	// vantage) — the post-ingest hook that closes CODE_REVIEW #5 / P2-5. nil = no alerting
	// on ingest (e.g. pure API tests).
	OnIngest func(outcomes []scheduler.Outcome)
	// IngestCommit, if set, SUPERSEDES the AddResults+OnIngest pair for the /agent/v1/results
	// path: it persists a snapshot-validated batch and evaluates its alerts atomically under the
	// runtime's reload boundary, re-checking each outcome's target identity against the LIVE
	// assignment first. This closes the window between the handler's snapshot validation and the
	// store write in which a config reload could redefine a target and leave a stale round stored
	// under the new name (CODE_REVIEW M4). It returns the rounds that survived that LIVE identity
	// gate (including idempotent replays); the handler uses this to report commit-time reload drops
	// accurately. Store deduplication and alerting over newly-inserted rows stay inside the callback.
	// nil ⇒ the handler falls back to AddResults + OnIngest (e.g. pure API tests).
	IngestCommit func(ctx context.Context, outcomes []scheduler.Outcome) (accepted []scheduler.Outcome, err error)
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
	// (heliograph_agent_missing_fingerprint_total) so an operator can watch a rolling agent
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
	// ConfigYAML, if set, renders the config as YAML for the read-only Config-tab view
	// (GET /api/admin/config.yaml?source=db|effective). source "db" is the stored DB fragment;
	// "effective" is the file+DB merged config the collector is running. Open read like ConfigGet;
	// when redact is true (a non-admin reader) secret-capable probe params are sanitized before
	// rendering (CODE_REVIEW M11). nil ⇒ route not registered.
	ConfigYAML func(source string, redact bool) ([]byte, error)
	// ConfigEffective, if set, returns the file+DB merged config as JSON — the read-only "effective"
	// source of GET /api/admin/config?source=effective, which the Config tab renders as a tree. Same
	// snapshot as ConfigYAML("effective"); redacted for a non-admin reader like ConfigGet (M11).
	// nil ⇒ the effective source answers 503.
	ConfigEffective func() (json.RawMessage, error)
	// ConfigRedact, if set, sanitizes secret-capable probe params in a config document (the HTTP
	// urlformat's userinfo/query) for the open reads. Applied to GET /api/admin/config (both the DB
	// and effective sources) only for a request without a valid admin session; a logged-in admin is
	// served the real, editable doc so a subsequent save can't overwrite a secret with its masked
	// form (CODE_REVIEW M11).
	ConfigRedact func(doc json.RawMessage) (json.RawMessage, error)
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
	// Order by the stable identity: a store-scanned outcome carries the id, not the display
	// Name (which is empty), so sorting on Name would be a no-op in production.
	sort.Slice(out, func(i, j int) bool { return out[i].Target.Key() < out[j].Target.Key() })
	return out, nil
}

func (srv *Server) Routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/version", srv.version)
	mux.HandleFunc("GET /api/probes", srv.probes)
	mux.HandleFunc("GET /api/probes/schema", srv.probeSchema)
	mux.HandleFunc("GET /api/charts", srv.charts)
	mux.HandleFunc("GET /api/sla", srv.sla)
	mux.HandleFunc("GET /api/targets", srv.targets)
	mux.HandleFunc("GET /api/series", srv.series)
	mux.HandleFunc("GET /api/series/all", srv.seriesAll)
	mux.HandleFunc("GET /api/rollup", srv.rollup)
	mux.HandleFunc("GET /metrics", srv.metrics)
	if srv.AdminPassword != "" && len(srv.AdminKey) > 0 {
		// Admin auth endpoints, available whenever admin is configured — independent of which
		// admin feature (vantages/config) is wired — so the top-bar login/logout + session probe
		// work on every tab.
		mux.HandleFunc("POST /api/admin/login", srv.adminLogin)
		mux.HandleFunc("POST /api/admin/logout", srv.adminLogout)
		mux.HandleFunc("GET /api/admin/session", srv.requireAdmin(srv.adminSession))
	}
	if srv.AdminPassword != "" && srv.Vantages != nil && len(srv.AdminKey) > 0 {
		// Read is open (behind the proxy's Basic Auth like the rest of the dashboard): listVantages
		// returns only names/created/last-seen/counts — never a key — so anyone who can see the graphs
		// can see which vantages exist. Minting and revoking stay admin-gated.
		mux.HandleFunc("GET /api/admin/vantages", srv.listVantages)
		mux.HandleFunc("POST /api/admin/vantages", srv.requireAdmin(srv.addVantage))
		mux.HandleFunc("DELETE /api/admin/vantages/{name}", srv.requireAdmin(srv.revokeVantage))
	}
	if srv.AdminPassword != "" && len(srv.AdminKey) > 0 && srv.ConfigGet != nil && srv.ConfigApply != nil {
		// Read is open like the vantage list: the config doc is the monitoring tree (targets, probes,
		// alert routing by notifier NAME). The environment-sourced secrets (DB password, webhook URLs)
		// are never in it, but a probe param CAN embed a credential (an HTTP urlformat's userinfo or
		// query), so a non-admin read is redacted; a logged-in admin gets the real doc (CODE_REVIEW
		// M11). Applying changes stays admin-gated.
		mux.HandleFunc("GET /api/admin/config", srv.getConfig)
		mux.HandleFunc("PUT /api/admin/config", srv.requireAdmin(srv.putConfig))
		if srv.ConfigYAML != nil {
			// Open read alongside GET /api/admin/config — same non-secret config, rendered as YAML.
			mux.HandleFunc("GET /api/admin/config.yaml", srv.getConfigYAML)
		}
	}
	if srv.AdminPassword != "" && len(srv.AdminKey) > 0 && srv.ConfigImport != nil {
		mux.HandleFunc("POST /api/admin/config/import", srv.requireAdmin(srv.importConfig))
	}
	// The agent routes (/agent/v1/assignment, /agent/v1/results) are not registered here:
	// requireAgent (agentauth.go) authorizes a caller by client-cert CN, but this mux is plain
	// HTTP, so it never wires it up. agentAssignment/agentResults stay as unexported methods —
	// the opt-in mTLS listener cmd/smoked starts when -agent-addr is set (AgentMux, defined in
	// this package's agentmtls.go) is what re-lights them behind requireAgent; single-host
	// deployments (the default, no -agent-addr) never expose them at all.
	if srv.webDir != "" {
		// Serve the SPA/static assets at the root (same-origin with the API).
		mux.Handle("GET /", noCacheStatic(http.FileServer(http.Dir(srv.webDir))))
	}
	return mux
}

// noCacheStatic wraps a static file handler so browsers always revalidate the
// dashboard assets before reusing them. FileServer emits only Last-Modified
// (no Cache-Control, no ETag), so without this a browser applies heuristic
// freshness and can keep running pre-upgrade dashboard.js/smoke.js against a
// freshly-deployed server until a hard refresh. "no-cache" doesn't forbid
// caching — it forces a conditional request (If-Modified-Since), which the
// FileServer answers with a cheap 304 when the file is unchanged and a fresh
// 200 the moment it changes. Assets are still cached; they're just never
// served stale after a deploy.
func noCacheStatic(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		h.ServeHTTP(w, r)
	})
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

// version reports the collector's build version for the dashboard footer.
func (srv *Server) version(w http.ResponseWriter, _ *http.Request) {
	v := srv.Version
	if v == "" {
		v = "dev"
	}
	writeJSON(w, map[string]any{"version": v, "absolute_time": srv.AbsoluteTime})
}

// probeSchema emits each probe's config variables as JSON Schema (draft 2020-12),
// generated from the same VarSpec source that drives runtime validation — so docs
// and external validators never drift from what the collector actually accepts.
func (srv *Server) probeSchema(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{"probes": probe.AllSchemas()})
}

type targetDTO struct {
	// ID is the target's stable storage key — the value to pass to /api/series?target= and to
	// key a deep link on. Name is the display path (the tree position), which a move changes
	// while ID stays put. For all existing targets ID == Name (the path was the key).
	ID   string `json:"id"`
	Name string `json:"name"`
	// Title is the display-name override (falls back to Name in the UI); IP is the address
	// to show in the graph title (pinned or resolved). Both display-only, omitted when unset.
	Title string `json:"title,omitempty"`
	IP    string `json:"ip,omitempty"`
	Probe string `json:"probe"`
	// Metric names what median_ms means: "rtt" (round-trip time; every probe but NTP-offset) or
	// "offset" (a signed clock offset). Config-derived (per-target `measure`, else the stored
	// round's kind), so the UI's signed-axis choice survives a restart or a target with no live
	// NTP stat — unlike ntp_measure, which comes from the process-local registry. Always set.
	Metric   string   `json:"metric"`
	MedianMs *float64 `json:"median_ms"`
	Loss     int      `json:"loss"`
	Pings    int      `json:"pings"`
	LossPct  float64  `json:"loss_pct"`
	// RecentLossPct is the average loss percent over a short recent window (not just the
	// last round), so the UI can key the target's status dot on sustained loss rather than a
	// single dropped ping. Omitted when the store can't aggregate it (falls back to LossPct).
	RecentLossPct *float64 `json:"recent_loss_pct,omitempty"`
	When          string   `json:"when"`
	Error         string   `json:"error,omitempty"`
	Vantages      []string `json:"vantages,omitempty"`
	// NTPOffsetMs + Stratum are NTP-only display stats (clock offset in ms, server stratum). They
	// are not latencies, so they never enter the RTT/loss pipeline; they come from the probe's
	// latest-value registry via Server.NTPStat. Omitted for non-NTP targets and before first data.
	// NTPMeasure ("rtt"/"offset") is the target's graphed metric, so the UI can hide the redundant
	// offset stat on a panel already graphing offset.
	NTPOffsetMs *float64 `json:"ntp_offset_ms,omitempty"`
	Stratum     *int     `json:"stratum,omitempty"`
	NTPMeasure  string   `json:"ntp_measure,omitempty"`
	// NoData marks a configured target that has no stored round for the requested
	// vantage yet — e.g. a remote-only target before its agent reports. Such a target
	// is listed (so it appears in the tree and its deep link resolves) but carries no
	// median/loss/when, and the UI shows a neutral status rather than a false "healthy".
	NoData bool `json:"no_data,omitempty"`
}

// TargetMeta is a target's display-only metadata: its title override and the IP to show
// in the graph title (pinned or resolved). Kept separate from measurement identity.
type TargetMeta struct {
	Title string
	IP    string
}

// recentStatusWindow is how far back the status-dot loss is averaged. Short enough to
// reflect current health, long enough that a single dropped ping doesn't dominate — so the
// nav dot keys on sustained loss rather than one lossy round.
const recentStatusWindow = 30 * time.Minute

// configuredMetric returns the metric a target's per-target params pin — probe.MetricOffset or
// probe.MetricRTT when `measure` is set, or "" when it is not (the caller then falls back to the
// stored round's kind, or rtt). NTP is the only probe that sets `measure`; every other probe
// leaves it unset and is rtt.
func configuredMetric(params map[string]string) string {
	if params == nil {
		return ""
	}
	if m, ok := params["measure"]; ok && m != "" {
		return probe.NormalizeMetric(m)
	}
	return ""
}

// monitorKey is a Monitor's stable storage identity — its ID, falling back to its display Name
// (path) when no id is set. It mirrors probe.Target.Key() so the catalog join, the series
// resolver, and the /metrics label all match the store, which keys history by the same fallback
// for an id-less target (a hand-built monitor, or config that predates node ids). In production
// buildRuntime backfills ID = Name before the catalog is exposed, so this is inert there; it keeps
// the API correct for monitors that reach it without an id (chiefly tests).
func monitorKey(m model.Monitor) string {
	if m.ID != "" {
		return m.ID
	}
	return m.Name
}

// displayPaths returns an id -> display path map from the live catalog, or nil when no catalog is
// wired. A store-scanned outcome carries only its id (Name is empty), so any listing that shows a
// human-facing name — the /metrics label, a charts entry, an sla entry — resolves the id through
// this map. Because id == path for every existing target, the resolved label is unchanged today.
func (srv *Server) displayPaths() map[string]string {
	if srv.Configured == nil {
		return nil
	}
	cfg := srv.Configured()
	paths := make(map[string]string, len(cfg))
	for _, m := range cfg {
		paths[monitorKey(m)] = m.Name
	}
	return paths
}

// displayName resolves a target's stable identity to its current display path via paths, falling
// back to the identity itself when the target is absent from the catalog (or no catalog is wired —
// paths is nil, a safe read that yields ""). Never returns "" for a target that has an identity.
func displayName(paths map[string]string, identity string) string {
	if p := paths[identity]; p != "" {
		return p
	}
	return identity
}

// latestDTO builds a target DTO from a stored latest round, attaching the target's vantage
// set, display metadata, and windowed recent loss (for the status dot). Shared by the live
// and catalog-driven listings. identity is the target's stable storage key (o.Target.Key());
// every side-map lookup (tv/meta/recent/ntp) keys by it, since a store-scanned outcome carries
// only the id, not the display name. displayName is the path shown to the user (from the live
// catalog), and probeKind is the authoritative probe identity for the panel — the catalog's
// current kind where known, so a stored round left behind under a reused id never mislabels the
// panel (M3); callers without a catalog pass the id as the display name and the stored round's kind.
// wantNTPKey is the target's current NTP measurement identity (ntpprobe.StatKey) used to gate the
// clock stat; "" skips that gate (non-NTP targets, or a no-catalog listing with no config drift).
func latestDTO(o scheduler.Outcome, identity, displayName, probeKind, wantNTPKey string, tv map[string][]string, meta map[string]TargetMeta, recent map[string]float64, ntp func(target, wantKey string) (float64, uint8, string, bool)) targetDTO {
	dto := targetDTO{
		ID:      identity,
		Name:    displayName,
		Probe:   probeKind,
		Metric:  store.MetricOf(o),
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
	if vs := tv[identity]; len(vs) > 0 {
		dto.Vantages = vs
	}
	if md, ok := meta[identity]; ok {
		dto.Title, dto.IP = md.Title, md.IP
	}
	if rl, ok := recent[identity]; ok {
		dto.RecentLossPct = &rl
	}
	// Only an NTP panel carries offset/stratum. Gate on the CURRENT probe identity (not the stored
	// round's) — an id reused for a different probe must not show the old NTP stat — and on the
	// target's current measurement identity: the accessor refuses a reading whose key no longer
	// matches wantNTPKey, so an id repointed at a different NTP server/port/version (or a slow
	// in-flight probe of the old endpoint) can't mislabel the new server with the old one's
	// offset/stratum (M3). The caller passes a per-vantage registry, since srv.NTPStat is the hub's
	// LOCAL reading and must never decorate a remote outcome.
	if ntp != nil && probeKind == "NTP" {
		if offSec, stratum, measure, ok := ntp(identity, wantNTPKey); ok {
			offMs := offSec * 1000
			st := int(stratum)
			dto.NTPOffsetMs, dto.Stratum, dto.NTPMeasure = &offMs, &st, measure
		}
	}
	return dto
}

func (srv *Server) targets(w http.ResponseWriter, r *http.Request) {
	v, ok := readVantage(w, r)
	if !ok {
		return
	}
	// The clock-stat source depends on the vantage: the hub's own registry (srv.NTPStat) for the
	// local vantage, the per-vantage store fed by RoundReports for a remote one. Never cross the
	// two — a remote NTP panel must show that vantage's server, not the hub's local reading (M2/M3).
	ntpStat := srv.NTPStat
	if v != store.DefaultVantage {
		ntpStat = srv.remoteNTP.lookupFor(v)
	}
	// The tree-wide probes.NTP config supplies the port/version defaults an NTP target's measurement
	// identity (ntpprobe.StatKey) resolves against, so the clock-stat gate matches what the probe
	// stored even when the endpoint is a probe-level default (M3). Guarded: bare setups/tests wire no
	// Assignment, and then the per-target params alone (StatKey's schema defaults) suffice.
	var ntpCfg map[string]string
	if srv.Assignment != nil {
		if _, probeCfgs, _ := srv.Assignment(v); probeCfgs != nil {
			ntpCfg = probeCfgs["NTP"]
		}
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
	var meta map[string]TargetMeta
	if srv.TargetMeta != nil {
		meta = srv.TargetMeta()
	}
	// Windowed recent loss per target for the status dot — one bulk scan, best-effort: if the
	// store can't aggregate it or the scan fails, the dot falls back to the last round's loss
	// rather than failing the whole listing.
	var recent map[string]float64
	if avAll, ok := srv.store.(store.AvailabilityAller); ok {
		if m, err := avAll.AvailabilityAll(r.Context(), v, time.Now().Add(-recentStatusWindow), nil); err == nil {
			recent = make(map[string]float64, len(m))
			for name, st := range m {
				if st.Measured > 0 {
					recent[name] = st.SumLossPct / float64(st.Measured)
				}
			}
		} else {
			slog.Warn("targets: recent-loss scan failed; status dots fall back to last round", "err", err)
		}
	}
	// Index the stored rounds by their stable identity (o.Target.Key()) — NOT the display Name,
	// which a store-scanned outcome leaves empty — so the catalog join below matches on m.ID.
	byID := make(map[string]scheduler.Outcome, len(latest))
	for _, o := range latest {
		byID[o.Target.Key()] = o
	}
	var out []targetDTO
	if srv.Configured != nil {
		// Catalog-driven: list every configured target so remote-only targets (no local
		// row) still appear and their deep links resolve; merge live data where present,
		// otherwise emit a no-data entry carrying the target's vantage set (CODE_REVIEW #3).
		// Identity (the store key + side-map key) is the Monitor's key; display (name/title) is the
		// Monitor's Name/Title.
		for _, m := range srv.Configured() {
			id := monitorKey(m)
			wantNTPKey := ""
			if m.ProbeKind == "NTP" {
				wantNTPKey = ntpprobe.StatKey(m.Host, m.Params, ntpCfg)
			}
			if o, ok := byID[id]; ok {
				dto := latestDTO(o, id, m.Name, m.ProbeKind, wantNTPKey, tv, meta, recent, ntpStat)
				// The effective config metric (probe-level default + per-target override) is
				// authoritative: it wins over the stored round's kind right after a config change and
				// honors a probe-level default. Falls back to the per-target param when no resolver
				// is wired (bare setups/tests).
				if em := srv.effectiveMetric(id); em != "" {
					dto.Metric = em
				} else if cm := configuredMetric(m.Params); cm != "" {
					dto.Metric = cm
				}
				out = append(out, dto)
				continue
			}
			dto := targetDTO{ID: id, Name: m.Name, Probe: m.ProbeKind, NoData: true, Metric: probe.MetricRTT}
			if em := srv.effectiveMetric(id); em != "" {
				dto.Metric = em
			} else if cm := configuredMetric(m.Params); cm != "" {
				dto.Metric = cm
			}
			if vs := tv[id]; len(vs) > 0 {
				dto.Vantages = vs
			} else if len(m.Vantages) > 0 {
				dto.Vantages = m.Vantages
			}
			if md, ok := meta[id]; ok {
				dto.Title, dto.IP = md.Title, md.IP
			}
			if rl, ok := recent[id]; ok {
				dto.RecentLossPct = &rl
			}
			out = append(out, dto)
		}
	} else {
		// No catalog (bare setups/tests): the id is the display path — they coincide when no
		// node id is set — so pass the stored round's identity as both.
		for _, o := range latest {
			key := o.Target.Key()
			// No catalog means the target's config can't have drifted, so the stored reading is always
			// current — pass "" to skip the identity gate (the probe stored under its measured endpoint,
			// which this listing can't independently reconstruct without config).
			out = append(out, latestDTO(o, key, key, o.ProbeName, "", tv, meta, recent, ntpStat))
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	}
	writeJSON(w, map[string]any{"targets": out})
}

type chartEntry struct {
	// ID is the target's stable storage key — the deep-link key that survives a move. Name is the
	// current display path (for the label). The Overview links by ID so a link built from this board
	// keeps working after the target is moved/renamed (CODE_REVIEW L8).
	ID       string   `json:"id"`
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

// measuredFromVantage reports whether a target with assigned vantage set `vs` is
// measured from `vant`. An empty/absent set means the implicit single-vantage default
// (local), mirroring seriesCatalog — so a legacy unscoped target is measured only from
// local. The Overview boards use this to drop a target from a vantage that doesn't
// measure it: the hub probes every configured target locally, so a target assigned only
// to a remote vantage still records 100%-loss LOCAL rounds (it's unreachable from the
// hub) that would otherwise read as a false worst-loss / zero-availability outage.
func measuredFromVantage(vs []string, vant string) bool {
	want := store.VantageOrDefault(vant)
	if len(vs) == 0 {
		return want == store.DefaultVantage
	}
	for _, v := range vs {
		if store.VantageOrDefault(v) == want {
			return true
		}
	}
	return false
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
	// A store-scanned outcome carries only its id, so take the display name from the live catalog
	// (id -> path); identity plays no part in ranking, only the label the user reads.
	paths := srv.displayPaths()
	// Drop targets this vantage doesn't measure, so a remote-only target's failed local rounds
	// don't rank as a worst-loss outage on the local Overview (see measuredFromVantage). Guarded:
	// no TargetVantages (bare setups/tests) means no scoping is known, so rank everything as before.
	var tv map[string][]string
	if srv.TargetVantages != nil {
		tv = srv.TargetVantages()
	}
	var rows []scored
	for _, o := range latest {
		if tv != nil && !measuredFromVantage(tv[o.Target.Key()], vant) {
			continue
		}
		e := chartEntry{
			ID:      o.Target.Key(),
			Name:    displayName(paths, o.Target.Key()),
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
			// The Latency board ranks round-trip time; a signed clock offset is not latency, so an
			// offset-mode target is excluded (its "median" is a µs offset that would rank as a
			// spuriously fast/slow host). Loss ranking below stays metric-agnostic.
			if e.MedianMs == nil || store.MetricOf(o) == probe.MetricOffset {
				continue // no latency to rank a fully-lost / offset target by
			}
			key = *e.MedianMs
		case "stddev":
			if e.StdDevMs == nil || store.MetricOf(o) == probe.MetricOffset {
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
	// ID is the stable deep-link key; Name is the current display path (see chartEntry, L8).
	ID           string   `json:"id"`
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
	// Identity for the store aggregates is the stable id (o.Target.Key()); the display name comes
	// from the live catalog (id -> path). A store-scanned outcome has an EMPTY Name, so keying the
	// aggregates on it would miss every target and return an empty report under the DB backend.
	paths := srv.displayPaths()
	// Drop targets this vantage doesn't measure (see charts / measuredFromVantage): a remote-only
	// target's failed local rounds would otherwise show as 0% availability on the local Overview.
	var tv map[string][]string
	if srv.TargetVantages != nil {
		tv = srv.TargetVantages()
	}
	out := make([]slaEntry, 0)
	for _, o := range active {
		id := o.Target.Key()
		if tv != nil && !measuredFromVantage(tv[id], vant) {
			continue
		}
		var (
			measured, up   int
			sumLoss        float64
			oldest, latest time.Time
		)
		switch {
		case statsAll != nil:
			st := statsAll[id] // zero value (measured 0) if no in-window rounds
			measured, up, sumLoss, oldest, latest = st.Measured, st.Up, st.SumLossPct, st.Oldest, st.Latest
		case av != nil:
			st, err := av.Availability(r.Context(), id, vant, cutoff, maxLossPct)
			if err != nil {
				slog.Warn("sla: availability query failed", "target", id, "err", err)
				continue
			}
			measured, up, sumLoss, oldest, latest = st.Measured, st.Up, st.SumLossPct, st.Oldest, st.Latest
		default:
			h, err := srv.store.History(id)
			if err != nil {
				slog.Warn("sla: history query failed", "target", id, "err", err)
				continue
			}
			measured, up, sumLoss, oldest, latest = slaOf(h, cutoff, isUp)
		}
		if measured == 0 {
			continue
		}
		e := slaEntry{
			ID:           id,
			Name:         displayName(paths, id),
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
		if step, ok := steps[id]; ok && step > 0 {
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
	// The param is the stable id; a pre-move bookmark may carry the old display path — resolve
	// it to the id so the store lookup (keyed by id) and the metric resolver both hit, and echo
	// the resolved key back in the response's "target".
	key = srv.resolveTargetKey(key)
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
		rounds, metric := currentMetricSeries(hist, srv.effectiveMetric(key))
		writeJSON(w, map[string]any{"target": key, "metric": metric, "rounds": roundsDTO(rounds)})
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
	rounds, metric := currentMetricSeries(hist, srv.effectiveMetric(key))
	writeJSON(w, map[string]any{"target": key, "metric": metric, "rounds": roundsDTO(rounds)})
}

// currentMetricSeries drops rounds whose metric differs from the target's current metric — a target
// that changed `measure` must not graph two meanings (rtt seconds and signed offset seconds) on one
// axis. `current` is the effective config metric when known (so a switch takes effect immediately);
// "" falls back to the newest stored round's metric. Returns the filtered rounds (oldest->newest)
// and the metric used, which the handler echoes so the client picks the matching axis.
func currentMetricSeries(hist []scheduler.Outcome, current string) ([]scheduler.Outcome, string) {
	if current == "" {
		if len(hist) == 0 {
			return hist, probe.MetricRTT
		}
		current = store.MetricOf(hist[len(hist)-1]) // History is oldest->newest
	}
	out := make([]scheduler.Outcome, 0, len(hist))
	for _, o := range hist {
		if store.MetricOf(o) == current {
			out = append(out, o)
		}
	}
	return out, current
}

// effectiveMetric returns the target's config-resolved metric, or "" when no resolver is wired
// (tests, or a bare setup) — the caller then falls back to the stored round's kind.
func (srv *Server) effectiveMetric(target string) string {
	if srv.EffectiveMetric != nil {
		return srv.EffectiveMetric(target)
	}
	return ""
}

// resolveTargetKey maps a /api/series target param to its storage key. The param is normally the
// stable id; a pre-move bookmark may instead carry the target's old display PATH. With a catalog
// wired, a param that is a known id is used as-is; otherwise a param matching a known display path
// is mapped to that target's id; anything else (bare setups, or an unknown target) is returned
// unchanged. An id match wins over a path match, so a param that is genuinely an id still resolves
// to itself even if it happens to equal another target's path. Because id == path for every
// existing target, this fallback is inert today and only matters for a link to a node's pre-move path.
func (srv *Server) resolveTargetKey(param string) string {
	if srv.Configured == nil {
		return param
	}
	var pathID string
	for _, m := range srv.Configured() {
		id := monitorKey(m)
		if id == param {
			return param
		}
		if pathID == "" && m.Name == param {
			pathID = id
		}
	}
	if pathID != "" {
		return pathID
	}
	return param
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
// materialize a huge response (1000 targets × 20k = 20M) — an unauthenticated memory/DoS sink on the
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

// seriesCatalog returns the bounded configured targets measured from vantage, as their stable
// storage ids (the key the store's SeriesAll queries by, and the key the response is grouped
// under). A catalog is required: falling back to Store.Keys would reintroduce an unbounded DISTINCT
// over persistent history on this unauthenticated endpoint, which is exactly what the catalog-driven
// query is intended to remove.
func (srv *Server) seriesCatalog(vantage string) ([]string, error) {
	if srv.Configured == nil {
		return nil, errors.New("configured target catalog unavailable")
	}
	want := store.VantageOrDefault(vantage)
	seen := map[string]bool{}
	var ids []string
	for _, m := range srv.Configured() {
		id := monitorKey(m)
		vantages := m.Vantages
		if len(vantages) == 0 {
			vantages = []string{store.DefaultVantage}
		}
		for _, v := range vantages {
			if v == want && !seen[id] {
				seen[id] = true
				ids = append(ids, id)
				break
			}
		}
	}
	sort.Strings(ids)
	return ids, nil
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
	catalog, err := srv.seriesCatalog(vant)
	if err != nil {
		slog.Error("series/all target catalog failed", "err", err)
		http.Error(w, `{"error":"series unavailable"}`, http.StatusServiceUnavailable)
		return
	}
	all, storeTruncated, err := sa.SeriesAll(r.Context(), vant, catalog, cutoff, maxSeriesAllTotalRounds)
	if err != nil {
		slog.Error("series/all query failed", "err", err)
		http.Error(w, `{"error":"series unavailable"}`, http.StatusServiceUnavailable)
		return
	}
	// The store already bounds the query to the global budget (keeping each target's newest rounds);
	// capSeriesAll is a cheap response-side backstop for a store that doesn't. Either counts as
	// truncation (CODE_REVIEW M5).
	truncated := storeTruncated || capSeriesAll(all, maxSeriesAllTotalRounds)
	// The store groups by (and the catalog is built from) the stable id, so each key here is a
	// target id — the same key /api/series and the DTO's `id` use. The client joins it against
	// /api/targets for the display path.
	targets := make(map[string]any, len(all))
	total := 0
	for id, hist := range all {
		rounds, metric := currentMetricSeries(hist, srv.effectiveMetric(id))
		total += len(rounds)
		targets[id] = map[string]any{"metric": metric, "rounds": roundsDTO(rounds)}
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
	// The param is the stable id; a pre-move bookmark or the frontend's live display path may carry
	// the target's display PATH — resolve it to the id so the store lookup (keyed by id) and the
	// metric resolver both hit, symmetric with /api/series (else a moved target's bands render blank).
	key = srv.resolveTargetKey(key)
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
	// A target that changed `measure` has buckets of both kinds (the aggregate groups by metric);
	// keep only the current one so the long-range view matches the raw series' axis and never mixes
	// rtt and offset. Prefer the effective config metric (a switch takes effect at once, and a bucket
	// holding both kinds has no defined order); fall back to the newest bucket's metric. Rollup
	// returns buckets ordered by time asc.
	current := srv.effectiveMetric(key)
	if current == "" {
		current = probe.MetricRTT
		if len(points) > 0 {
			current = points[len(points)-1].Metric
		}
	}
	buckets := make([]rollupDTO, 0, len(points))
	for _, p := range points {
		if p.Metric != current {
			continue
		}
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
	writeJSON(w, map[string]any{"target": key, "resolution": res, "metric": current, "buckets": buckets})
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
	// Prometheus labels are for dashboards and should read as the target's CURRENT path, not its
	// opaque storage id. A store-scanned outcome carries only the id, so resolve it to the display
	// path via the live catalog; an id with no catalog entry (or no catalog wired) keeps the id as
	// its label. Because id == path for every existing target, scrape/Grafana labels are unchanged —
	// this only relabels a moved node to its new path.
	paths := srv.displayPaths()
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	var b strings.Builder
	b.WriteString("# HELP heliograph_probe_median_seconds Median round-trip time of the most recent round.\n")
	b.WriteString("# TYPE heliograph_probe_median_seconds gauge\n")
	b.WriteString("# HELP heliograph_ntp_offset_seconds Most recent NTP clock offset in seconds (signed; offset-mode NTP targets only).\n")
	b.WriteString("# TYPE heliograph_ntp_offset_seconds gauge\n")
	b.WriteString("# HELP heliograph_probe_loss_ratio Fraction of pings lost in the most recent round (0..1).\n")
	b.WriteString("# TYPE heliograph_probe_loss_ratio gauge\n")
	b.WriteString("# HELP heliograph_probe_up 1 if the most recent round got at least one reply, else 0.\n")
	b.WriteString("# TYPE heliograph_probe_up gauge\n")
	b.WriteString("# HELP heliograph_probe_duration_seconds Wall-clock the most recent measurement of this target took.\n")
	b.WriteString("# TYPE heliograph_probe_duration_seconds gauge\n")
	b.WriteString("# HELP heliograph_probe_last_sample_timestamp_seconds Unix time of this target's most recent round (alert on staleness).\n")
	b.WriteString("# TYPE heliograph_probe_last_sample_timestamp_seconds gauge\n")
	for _, o := range latest {
		lbl := fmt.Sprintf(`{target=%q,probe=%q}`, escapeLabel(displayName(paths, o.Target.Key())), escapeLabel(o.ProbeName))
		median := o.Computed.Median
		// A signed clock offset is not a round-trip time, so it never rides the RTT median gauge
		// (whose HELP says "round-trip time") — it goes to an offset-specific gauge instead. Loss,
		// up, duration and staleness below are metric-agnostic and stay for every target.
		if store.MetricOf(o) == probe.MetricOffset {
			if !math.IsNaN(median) {
				fmt.Fprintf(&b, "heliograph_ntp_offset_seconds%s %g\n", lbl, median)
			}
		} else if math.IsNaN(median) {
			fmt.Fprintf(&b, "heliograph_probe_median_seconds%s NaN\n", lbl)
		} else {
			fmt.Fprintf(&b, "heliograph_probe_median_seconds%s %g\n", lbl, median)
		}
		fmt.Fprintf(&b, "heliograph_probe_loss_ratio%s %g\n", lbl, o.Computed.LossFraction())
		up := 0
		if o.Computed.Loss < o.Computed.Pings {
			up = 1
		}
		fmt.Fprintf(&b, "heliograph_probe_up%s %d\n", lbl, up)
		fmt.Fprintf(&b, "heliograph_probe_duration_seconds%s %g\n", lbl, o.Duration.Seconds())
		fmt.Fprintf(&b, "heliograph_probe_last_sample_timestamp_seconds%s %d\n", lbl, o.When.Unix())
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
// (heliograph_agent_missing_fingerprint_total) so an operator can tell when every vantage's agent
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
	b.WriteString("# HELP heliograph_agent_missing_fingerprint_total Agent rounds received with no measurement fingerprint (a pre-fingerprint agent): accepted-unverified in the lenient default, or dropped in strict mode (-require-fingerprint).\n")
	b.WriteString("# TYPE heliograph_agent_missing_fingerprint_total counter\n")
	for _, v := range vantages {
		fmt.Fprintf(b, "heliograph_agent_missing_fingerprint_total{vantage=%q} %d\n", escapeLabel(v), srv.missingFP[v])
	}
}

// writeRoundMetrics appends collector round-level operational metrics, if the
// server was given a RoundStats and at least one round has completed.
func (srv *Server) writeRoundMetrics(b *strings.Builder) {
	rs, ok := srv.Rounds.snapshot()
	if !ok {
		return
	}
	b.WriteString("# HELP heliograph_rounds_total Measurement rounds completed since start.\n")
	b.WriteString("# TYPE heliograph_rounds_total counter\n")
	fmt.Fprintf(b, "heliograph_rounds_total %d\n", rs.total)
	b.WriteString("# HELP heliograph_round_duration_seconds Wall-clock of the most recent round.\n")
	b.WriteString("# TYPE heliograph_round_duration_seconds gauge\n")
	fmt.Fprintf(b, "heliograph_round_duration_seconds %g\n", rs.duration.Seconds())
	b.WriteString("# HELP heliograph_round_targets Targets measured in the most recent round.\n")
	b.WriteString("# TYPE heliograph_round_targets gauge\n")
	fmt.Fprintf(b, "heliograph_round_targets %d\n", rs.targets)
	b.WriteString("# HELP heliograph_round_errors Targets that errored in the most recent round.\n")
	b.WriteString("# TYPE heliograph_round_errors gauge\n")
	fmt.Fprintf(b, "heliograph_round_errors %d\n", rs.errs)
	b.WriteString("# HELP heliograph_last_round_timestamp_seconds Start time of the most recent round.\n")
	b.WriteString("# TYPE heliograph_last_round_timestamp_seconds gauge\n")
	fmt.Fprintf(b, "heliograph_last_round_timestamp_seconds %d\n", rs.lastUnix)
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
	ttl := srv.AdminSessionTTL
	if ttl <= 0 {
		ttl = DefaultAdminSessionTTL
	}
	w.Header().Set("Cache-Control", "no-store") // never cache a session-issuing response
	http.SetCookie(w, &http.Cookie{
		Name: adminCookie, Value: signSession(srv.AdminKey, time.Now().Add(ttl)),
		Path: "/api/admin", HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode, MaxAge: int(ttl.Seconds()),
	})
	w.WriteHeader(http.StatusNoContent)
}

// adminLogout clears the admin session cookie (the cookie is HttpOnly, so the browser can't
// clear it from JS — the client hits this to log out). Idempotent; no auth required, since
// presenting no/stale credentials to log out is harmless.
func (srv *Server) adminLogout(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	http.SetCookie(w, &http.Cookie{
		Name: adminCookie, Value: "", Path: "/api/admin",
		HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode, MaxAge: -1,
	})
	w.WriteHeader(http.StatusNoContent)
}

// adminSession is a whoami probe behind requireAdmin: 204 when the session cookie is valid,
// 401 when not (and 404 when admin is disabled, since the route isn't registered). The top bar
// uses it to decide whether to show "Log in" or "Admin · Log out" on any tab.
func (srv *Server) adminSession(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

// hasAdminSession reports whether the request carries a currently-valid admin session cookie. It is
// the read-side of requireAdmin: the open config reads use it to serve the real document to a
// logged-in admin (who needs it to edit) while redacting secrets for the public (CODE_REVIEW M11).
// Safe because the config routes are registered only when admin is configured (a real AdminKey), so
// an empty-key forgery can never pass here.
func (srv *Server) hasAdminSession(r *http.Request) bool {
	c, err := r.Cookie(adminCookie)
	return err == nil && verifySession(srv.AdminKey, c.Value, time.Now())
}

func (srv *Server) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if !srv.hasAdminSession(r) {
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
	// federation_ready reports whether the hub actually runs an mTLS agent listener
	// (-agent-hostname set → AgentHubURL populated). When false, a minted bundle embeds a
	// placeholder hub URL and can never connect, so the UI disables onboarding rather than
	// handing out a dead bundle (CODE_REVIEW M19).
	writeJSON(w, map[string]any{"vantages": out, "federation_ready": srv.AgentHubURL != ""})
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
	wantsBundle := strings.Contains(r.Header.Get("Accept"), "application/gzip") || r.URL.Query().Get("format") == "bundle"
	// Refuse to mint an onboarding BUNDLE when the hub runs no agent listener: without
	// -agent-hostname the bundle's hub URL is only a placeholder (https://HUB-HOSTNAME:8443) and the
	// agent could never connect. The dashboard already disables onboarding via federation_ready, but
	// the CLI, a per-row Regenerate, and direct API callers reach here too — this is the real
	// enforcement point, so a dead "ready-to-run" bundle is never handed out (CODE_REVIEW M20,
	// completing M19). Refuse BEFORE Register/IssueClientCert so no state is created for a vantage
	// that can't be onboarded yet.
	if wantsBundle && srv.AgentHubURL == "" {
		http.Error(w, `{"error":"no agent listener configured — set -agent-hostname (SMOKED_AGENT_HOSTNAME) and restart before onboarding a vantage"}`, http.StatusConflict)
		return
	}
	if err := srv.Vantages.Register(r.Context(), body.Name); err != nil {
		switch {
		case errors.Is(err, vantage.ErrReserved):
			http.Error(w, `{"error":"\"local\" is reserved for the hub"}`, http.StatusConflict)
		case errors.Is(err, vantage.ErrInvalidName):
			http.Error(w, `{"error":"invalid vantage name (use letters, digits, . _ -)"}`, http.StatusBadRequest)
		default:
			slog.Error("addVantage: store register failed", "err", err)
			http.Error(w, `{"error":"store unavailable"}`, http.StatusServiceUnavailable)
		}
		return
	}
	// Mint the vantage's mTLS client identity right away, so the one-click "add vantage" admin
	// flow can hand back a ready-to-run onboarding bundle in the same response. This is the ONE
	// place the admin API carries private key material: mint time is the only moment smoked
	// itself holds the client key, so it must be returned here or nowhere (spec §3b). The CLI
	// (`smoked vantage add <name>`) mints the same way and prints the same PEM bundle to stdout;
	// both paths are equally admin/operator-gated.
	certPEM, keyPEM, caPEM, err := srv.Vantages.IssueClientCert(r.Context(), body.Name)
	if err != nil {
		slog.Error("addVantage: issue client cert failed", "name", body.Name, "err", err)
		http.Error(w, `{"error":"mint failed"}`, http.StatusInternalServerError)
		return
	}

	hub := srv.AgentHubURL
	if hub == "" {
		hub = "https://HUB-HOSTNAME:8443"
	}

	if wantsBundle {
		w.Header().Set("Content-Type", "application/gzip")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", body.Name+"-vantage.tar.gz"))
		// Headers (and the 200 status) are committed on the first Write below — a bundle-encoding
		// failure past that point can only be logged, not turned into an error status.
		if err := vantage.WriteBundleTarGz(w, hub, body.Name, certPEM, keyPEM, caPEM); err != nil {
			slog.Error("addVantage: write bundle failed", "name", body.Name, "err", err)
		}
		return
	}
	writeJSON(w, map[string]any{
		"name":        body.Name,
		"registered":  true,
		"hub":         hub,
		"client_cert": string(certPEM),
		"client_key":  string(keyPEM),
		"ca_cert":     string(caPEM),
	})
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

// redactConfigForReader returns doc unchanged for a logged-in admin (who needs the real, editable
// values), or with secret-capable probe params sanitized for any other reader — the open config
// reads must not disclose a credential embedded in a probe param, e.g. an HTTP urlformat's
// userinfo/query (CODE_REVIEW M11). A nil ConfigRedact (unwired) passes doc through unchanged.
func (srv *Server) redactConfigForReader(r *http.Request, doc json.RawMessage) (json.RawMessage, error) {
	if srv.ConfigRedact == nil || srv.hasAdminSession(r) {
		return doc, nil
	}
	return srv.ConfigRedact(doc)
}

func (srv *Server) getConfig(w http.ResponseWriter, r *http.Request) {
	// source=effective serves the file+DB merged config, read-only (the Config tab's default view).
	// It has no editable version — the frontend renders it read-only and edits only the DB fragment.
	if r.URL.Query().Get("source") == "effective" {
		if srv.ConfigEffective == nil {
			http.Error(w, `{"error":"effective config unavailable"}`, http.StatusServiceUnavailable)
			return
		}
		doc, err := srv.ConfigEffective()
		if err != nil {
			http.Error(w, `{"error":"config unavailable"}`, http.StatusServiceUnavailable)
			return
		}
		if trimmed := bytes.TrimSpace(doc); len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
			doc = json.RawMessage(`{}`)
		}
		// The effective config merges file-defined targets, which can carry credential-bearing probe
		// params too — redact for a non-admin reader like the DB source below (CODE_REVIEW M11).
		if doc, err = srv.redactConfigForReader(r, doc); err != nil {
			http.Error(w, `{"error":"config unavailable"}`, http.StatusServiceUnavailable)
			return
		}
		writeJSON(w, map[string]any{"readonly": true, "doc": doc})
		return
	}
	doc, version, err := srv.ConfigGet()
	if err != nil {
		http.Error(w, `{"error":"store unavailable"}`, http.StatusServiceUnavailable)
		return
	}
	if trimmed := bytes.TrimSpace(doc); len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		doc = json.RawMessage(`{}`)
	}
	// The config read is open, but a non-admin viewer must not see a credential in a probe param. A
	// logged-in admin gets the real doc: this is the editable source, so serving a masked value would
	// let the next save persist the mask over the secret (CODE_REVIEW M11).
	if doc, err = srv.redactConfigForReader(r, doc); err != nil {
		http.Error(w, `{"error":"config unavailable"}`, http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, map[string]any{"version": version, "doc": doc})
}

// getConfigYAML serves the config as read-only YAML for the Config tab's YAML view.
// source=db renders the stored DB fragment; source=effective renders the file+DB merged
// config the collector is running. Default db. Like getConfig it is an open read.
func (srv *Server) getConfigYAML(w http.ResponseWriter, r *http.Request) {
	source := r.URL.Query().Get("source")
	if source == "" {
		source = "db"
	}
	if source != "db" && source != "effective" {
		http.Error(w, `{"error":"source must be db or effective"}`, http.StatusBadRequest)
		return
	}
	// Redact secret-capable probe params for a non-admin reader; a logged-in admin sees the real
	// config (CODE_REVIEW M11). The YAML view is display-only (there is no YAML save), so unlike
	// getConfig there is no round-trip risk — this mirrors it only for a consistent auth model.
	out, err := srv.ConfigYAML(source, !srv.hasAdminSession(r))
	if err != nil {
		http.Error(w, `{"error":"config unavailable"}`, http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write(out)
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
