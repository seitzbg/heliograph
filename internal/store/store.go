// Package store defines the time-series sink interface and an in-memory
// implementation. The persistent implementation lives in store/pgstore
// (TimescaleDB). Keeping raw per-round samples is deliberate: the smoke bands
// are computed from the distribution, not just the median (see codemap 07 §4).
package store

import (
	"context"
	"errors"
	"regexp"
	"strconv"
	"sync"
	"time"

	"smokeping-modern/internal/scheduler"
)

// vantageNameRe bounds a vantage identifier: an alphanumeric start, then up to 63 more of
// alphanumeric / dot / dash / underscore. Lives here (a pgx-free package config, the key
// store, and the read API all import) so the same rule validates a name everywhere and the
// agent binary doesn't transitively link the database driver just to check a name.
var vantageNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`)

// ValidVantageName reports whether name is an acceptable vantage identifier — the single
// source of truth shared by config loading, key minting, and vantage-scoped reads, so a
// name that can never be provisioned (e.g. "new york") is rejected at config load rather
// than leaving a target permanently dark (CODE_REVIEW #11 / P3-11).
func ValidVantageName(name string) bool { return vantageNameRe.MatchString(name) }

// ErrRollupUnavailable is returned by a Rollupper whose downsampled aggregate has
// not been created (e.g. a PostgreSQL store started without -downsample, so the
// samples_hourly view is missing). The API turns it into a 501 so the UI disables
// hourly mode, rather than a 503 that looks like a transient failure.
var ErrRollupUnavailable = errors.New("store: rollup aggregate not available")

// DefaultVantage is the hub's own vantage name — the source of every locally probed
// round. Agent-sourced rounds carry their own vantage.
const DefaultVantage = "local"

// VantageOf returns o.Vantage, or DefaultVantage when it is empty, so the default lives
// in exactly one place across the in-memory and PostgreSQL stores.
func VantageOf(o scheduler.Outcome) string {
	return VantageOrDefault(o.Vantage)
}

// VantageOrDefault normalizes a read-side vantage selector: empty means the hub's
// own DefaultVantage ("local"). The write-side counterpart is VantageOf.
func VantageOrDefault(v string) string {
	if v == "" {
		return DefaultVantage
	}
	return v
}

// Store is the sink the collector writes each round's outcomes to, and the API
// reads series back from.
type Store interface {
	Add(outcomes []scheduler.Outcome)
	Keys() ([]string, error)
	Latest(key string) (scheduler.Outcome, bool)
	History(key string) ([]scheduler.Outcome, error)
}

// ResultIngester is implemented by stores that accept an agent-ingested batch and
// report the write error, so the ingest endpoint can answer 503 (agent retries)
// rather than silently dropping under a transient store failure. The local
// collector keeps the fire-and-forget Add; only ingest needs the feedback.
type ResultIngester interface {
	// AddResults persists a batch idempotently by (target, vantage, ts) and returns the
	// subset of outcomes that were NEWLY persisted (not already present). The caller
	// evaluates alerts only over that subset, so a replayed round — an HTTP retry, or the
	// deliberate resend when a split batch's later half fails transiently — is stored once
	// and never double-advances alert hysteresis (CODE_REVIEW #4/replay). A non-nil error
	// means the whole batch is uncertain; nothing should be treated as persisted.
	AddResults(ctx context.Context, outcomes []scheduler.Outcome) ([]scheduler.Outcome, error)
}

// RollupPoint is one downsampled bucket for a target (hourly or daily). Median
// values are NaN for buckets that were entirely lost.
//
// Rounds is the total rounds in the bucket (weight for loss); MedianRounds is the count
// of rounds that produced a median — i.e. not fully lost (weight for the median stats).
// They differ during an outage, and weighting the median by Rounds instead of MedianRounds
// biases a bucket's median toward its few surviving rounds (CODE_REVIEW #6 / P2-6).
type RollupPoint struct {
	Bucket       time.Time
	MedianAvg    float64
	MedianMin    float64
	MedianMax    float64
	LossFrac     float64
	Rounds       int
	MedianRounds int
}

// Rollupper is implemented by stores that support downsampled reads (the hourly
// and daily continuous aggregates). resolution selects the tier: "1h" (default)
// or "1d" — the daily tier feeds the long-range (400d) drill-down, where the
// hourly tier would be too many buckets. since/until bound the result to buckets in
// [since, until]; a zero since means "from the start" and a zero until means "through
// now", so a 10d view fetches ~240 hourly buckets server-side rather than the whole
// retained history, and a drag-zoom fetches an arbitrary historical sub-range. The API
// exposes it at /api/rollup.
type Rollupper interface {
	Rollup(ctx context.Context, target, vantage, resolution string, since, until time.Time) ([]RollupPoint, error)
}

// AvailabilityStat is the aggregate a store computes over a time window: how many
// rounds were measured (in the window), how many were "up", the summed loss
// percent (for an average), and the oldest/newest round actually seen. The API
// derives availability = Up/Measured, and coverage = Measured against the rounds
// it expected over the window from the target's step.
type AvailabilityStat struct {
	Measured   int
	Up         int
	SumLossPct float64
	Oldest     time.Time
	Latest     time.Time
}

// Availabler is implemented by stores that can aggregate availability over an
// arbitrary window — crucially, unbounded by the History cap, so a 24h SLA is
// computed over the whole 24h rather than the last N stored rounds. maxLossPct nil
// means "up" is at least one reply (loss < pings); non-nil means up is loss
// percent <= *maxLossPct. The API prefers this over the History-based fallback.
type Availabler interface {
	Availability(ctx context.Context, target, vantage string, cutoff time.Time, maxLossPct *float64) (AvailabilityStat, error)
}

// RangeHistorier is implemented by stores that can return a target's rounds over
// an arbitrary window (at or after cutoff), unbounded by the History cap — so a
// long drill-down range (e.g. 30h of per-round samples) isn't silently truncated
// to the last N stored rounds. Returned oldest->newest, like History. /api/series
// prefers this when given a window, falling back to the capped History otherwise.
// HistoryBetween is the same but bounded on both sides ([from, to]) — the drag-zoom
// path fetches an arbitrary historical sub-range, not just "the last window".
type RangeHistorier interface {
	HistorySince(ctx context.Context, target, vantage string, cutoff time.Time) ([]scheduler.Outcome, error)
	HistoryBetween(ctx context.Context, target, vantage string, from, to time.Time) ([]scheduler.Outcome, error)
}

// RecentHistorier returns a target's most recent rounds for a specific vantage, capped to
// the configured history cap — like History, but vantage-aware. The no-window /api/series
// path uses it so ?vantage=nyc returns nyc's recent rounds instead of the local-only
// History (CODE_REVIEW #7 / P2-7). Returned oldest->newest, like History.
type RecentHistorier interface {
	HistoryVantage(ctx context.Context, target, vantage string) ([]scheduler.Outcome, error)
}

// LatestAller returns every target's most recent outcome in one call. The live
// endpoints (targets, charts, metrics) prefer it over one Latest per target, so a
// refresh is a single query instead of N (the #5 query fan-out reduction). It
// returns an error so a backing-store read failure surfaces as an API 503 rather
// than a false-empty "0 targets" success (CODE_REVIEW #4).
type LatestAller interface {
	LatestAll(vantage string) (map[string]scheduler.Outcome, error)
}

// AvailabilityAller aggregates availability for every target over the window in one
// call — the bulk form of Availability, so /api/sla is one query instead of N.
type AvailabilityAller interface {
	AvailabilityAll(ctx context.Context, vantage string, cutoff time.Time, maxLossPct *float64) (map[string]AvailabilityStat, error)
}

// SeriesAller returns every target's rounds strictly after cutoff in one call — the
// bulk, incremental form of History that powers the Graphs grid. Instead of one
// /api/series per target per refresh, the grid makes a single query, and with
// cutoff = the newest round timestamp it has already seen (its watermark) it
// transfers only rounds newer than that, not the whole window every tick. Strictly
// after (not at) cutoff, so an incremental fetch never re-sends the watermark round.
// Targets with no rounds after cutoff are omitted; returned oldest->newest per target.
// The error lets a backing-store failure surface as an API 503, not a false-empty view.
type SeriesAller interface {
	SeriesAll(ctx context.Context, vantage string, cutoff time.Time) (map[string][]scheduler.Outcome, error)
}

// vk is the composite key MemStore's latest/history maps are indexed by, so rounds
// from different vantages for the same target never mix in memory (mirroring the
// pgstore samples_target_vantage_ts uniqueness).
type vk struct{ vantage, target string }

// MemStore is the in-memory implementation: latest + bounded history per
// (vantage, target). Used by default and in tests; not durable.
type MemStore struct {
	mu      sync.RWMutex
	latest  map[vk]scheduler.Outcome
	history map[vk][]scheduler.Outcome
	keys    []string // distinct target names, insertion order (for Keys())
	seen    map[string]bool
	// ingested records (vantage,target,ts) already accepted via AddResults, so a replayed
	// agent round is stored once — the dev/test analogue of the DB's unique constraint, so
	// MemStore doesn't double-count a replay for alert evaluation (CODE_REVIEW #4/replay).
	ingested map[string]bool
	cap      int
}

func NewMem(historyCap int) *MemStore {
	if historyCap <= 0 {
		historyCap = 512
	}
	return &MemStore{
		latest:   map[vk]scheduler.Outcome{},
		history:  map[vk][]scheduler.Outcome{},
		seen:     map[string]bool{},
		ingested: map[string]bool{},
		cap:      historyCap,
	}
}

func (s *MemStore) Add(outcomes []scheduler.Outcome) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, o := range outcomes {
		s.storeOne(o)
	}
}

// storeOne records one outcome into latest/history/keys. The caller holds s.mu. Vantage is
// resolved here so callers pass raw outcomes. Shared by Add (fire-and-forget local writes)
// and AddResults (deduplicated agent ingest).
func (s *MemStore) storeOne(o scheduler.Outcome) {
	o.Vantage = VantageOf(o) // resolve "" -> local once, on write
	k := vk{o.Vantage, o.Target.Name}
	if !s.seen[o.Target.Name] {
		s.seen[o.Target.Name] = true
		s.keys = append(s.keys, o.Target.Name)
	}
	s.latest[k] = o
	h := append(s.history[k], o)
	if len(h) > s.cap {
		// The round scrolling out of the capped window also leaves the replay index, so
		// `ingested` stays bounded to the retained history instead of growing with every round
		// ever seen (CODE_REVIEW: MemStore replay index). Matching retention means a replay older
		// than the window is re-accepted — fine for this dev/test store; the DB enforces true
		// uniqueness. (A no-op for local rounds, which never enter `ingested`.)
		delete(s.ingested, resultKey(h[0]))
		h = h[len(h)-s.cap:]
	}
	s.history[k] = h
}

// resultKey identifies one ingested round by (vantage, target, ts) — the same tuple the DB's
// unique constraint uses — so a replay is detected regardless of resolved-vantage timing.
func resultKey(o scheduler.Outcome) string {
	return VantageOf(o) + "\x00" + o.Target.Name + "\x00" + strconv.FormatInt(o.When.UTC().UnixNano(), 10)
}

// AddResults implements ResultIngester: it stores each round at most once (keyed by
// vantage,target,ts) and returns only the newly-stored subset, so a replayed agent round
// isn't double-counted for alert evaluation — the dev/test analogue of the DB's ON CONFLICT
// DO NOTHING (CODE_REVIEW #4/replay). MemStore is not durable, so this never errors.
func (s *MemStore) AddResults(_ context.Context, outcomes []scheduler.Outcome) ([]scheduler.Outcome, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var inserted []scheduler.Outcome
	for _, o := range outcomes {
		key := resultKey(o)
		if s.ingested[key] {
			continue // already stored — a replay
		}
		s.ingested[key] = true
		s.storeOne(o)
		inserted = append(inserted, o)
	}
	return inserted, nil
}

func (s *MemStore) Keys() ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, len(s.keys))
	copy(out, s.keys)
	return out, nil // in-memory reads never fail
}

// Latest returns the target's most recent LOCAL round — the hub's own vantage.
// Agent-ingested rounds from other vantages are reached via LatestAll(vantage).
func (s *MemStore) Latest(key string) (scheduler.Outcome, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	o, ok := s.latest[vk{DefaultVantage, key}]
	return o, ok
}

// History returns the target's recent LOCAL rounds — the hub's own vantage. Agent-
// ingested rounds from other vantages are reached via HistoryVantage/HistorySince.
func (s *MemStore) History(key string) ([]scheduler.Outcome, error) {
	return s.HistoryVantage(context.Background(), key, DefaultVantage)
}

// HistoryVantage returns the target's recent rounds for a specific vantage (P2-7).
func (s *MemStore) HistoryVantage(_ context.Context, target, vantage string) ([]scheduler.Outcome, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	h := s.history[vk{VantageOrDefault(vantage), target}]
	out := make([]scheduler.Outcome, len(h))
	copy(out, h)
	return out, nil // in-memory reads never fail
}

// Availability scans the in-memory history for target and aggregates the rounds at
// or after cutoff. Unlike a database store it can only see what it currently holds
// (bounded by the history cap) — best-effort, honest coverage; production uses the
// pgstore implementation, which is unbounded.
func (s *MemStore) Availability(_ context.Context, target, vantage string, cutoff time.Time, maxLossPct *float64) (AvailabilityStat, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var st AvailabilityStat
	for _, o := range s.history[vk{VantageOrDefault(vantage), target}] {
		if o.When.Before(cutoff) {
			continue
		}
		lossPct := o.Computed.LossFraction() * 100
		st.Measured++
		st.SumLossPct += lossPct
		up := lossPct < 100 // default: at least one reply
		if maxLossPct != nil {
			up = lossPct <= *maxLossPct
		}
		if up {
			st.Up++
		}
		if st.Oldest.IsZero() || o.When.Before(st.Oldest) {
			st.Oldest = o.When
		}
		if o.When.After(st.Latest) {
			st.Latest = o.When
		}
	}
	return st, nil
}

// HistorySince returns the target's rounds at or after cutoff, oldest->newest.
// Best-effort: MemStore only holds what fits its history cap, so a window longer
// than the cap covers is reported as far back as it can (honest, not truncated
// silently the way a DB LIMIT would be); production uses the pgstore implementation,
// which reads the full window.
func (s *MemStore) HistorySince(_ context.Context, target, vantage string, cutoff time.Time) ([]scheduler.Outcome, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	h := s.history[vk{VantageOrDefault(vantage), target}]
	out := make([]scheduler.Outcome, 0, len(h))
	for _, o := range h {
		if o.When.Before(cutoff) {
			continue
		}
		out = append(out, o)
	}
	return out, nil
}

// HistoryBetween returns the target's rounds in [from, to], oldest->newest.
// Best-effort like HistorySince, bounded by the in-memory cap.
func (s *MemStore) HistoryBetween(_ context.Context, target, vantage string, from, to time.Time) ([]scheduler.Outcome, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]scheduler.Outcome, 0)
	for _, o := range s.history[vk{VantageOrDefault(vantage), target}] {
		if o.When.Before(from) || o.When.After(to) {
			continue
		}
		out = append(out, o)
	}
	return out, nil
}

// LatestAll returns a copy of every target's most recent outcome for the given
// vantage. In-memory reads never fail, so the error is always nil.
func (s *MemStore) LatestAll(vantage string) (map[string]scheduler.Outcome, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	want := VantageOrDefault(vantage)
	out := make(map[string]scheduler.Outcome, len(s.latest))
	for k, o := range s.latest {
		if k.vantage == want {
			out[k.target] = o
		}
	}
	return out, nil
}

// SeriesAll returns every target's rounds strictly after cutoff, grouped by target,
// oldest->newest (history is kept in insertion order). Best-effort like History,
// bounded by the in-memory cap; production uses the pgstore implementation. Targets
// with no rounds after cutoff are omitted.
func (s *MemStore) SeriesAll(_ context.Context, vantage string, cutoff time.Time) (map[string][]scheduler.Outcome, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	want := VantageOrDefault(vantage)
	out := make(map[string][]scheduler.Outcome)
	for k, h := range s.history {
		if k.vantage != want {
			continue
		}
		var rounds []scheduler.Outcome
		for _, o := range h {
			if o.When.After(cutoff) {
				rounds = append(rounds, o)
			}
		}
		if len(rounds) > 0 {
			out[k.target] = rounds
		}
	}
	return out, nil
}

// AvailabilityAll aggregates every target over [cutoff, now) — the same best-effort
// scan as Availability, once per target it holds.
func (s *MemStore) AvailabilityAll(ctx context.Context, vantage string, cutoff time.Time, maxLossPct *float64) (map[string]AvailabilityStat, error) {
	s.mu.RLock()
	keys := make([]string, len(s.keys))
	copy(keys, s.keys)
	s.mu.RUnlock()
	out := make(map[string]AvailabilityStat, len(keys))
	for _, k := range keys {
		st, err := s.Availability(ctx, k, vantage, cutoff, maxLossPct)
		if err != nil {
			return nil, err
		}
		if st.Measured > 0 {
			out[k] = st
		}
	}
	return out, nil
}

// compile-time checks
var (
	_ Store             = (*MemStore)(nil)
	_ ResultIngester    = (*MemStore)(nil)
	_ Availabler        = (*MemStore)(nil)
	_ RangeHistorier    = (*MemStore)(nil)
	_ RecentHistorier   = (*MemStore)(nil)
	_ LatestAller       = (*MemStore)(nil)
	_ AvailabilityAller = (*MemStore)(nil)
	_ SeriesAller       = (*MemStore)(nil)
)
