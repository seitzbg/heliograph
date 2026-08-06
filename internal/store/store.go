// Package store defines the time-series sink interface and an in-memory
// implementation. The persistent implementation lives in store/pgstore
// (TimescaleDB). Keeping raw per-round samples is deliberate: the smoke bands
// are computed from the distribution, not just the median (see codemap 07 §4).
package store

import (
	"context"
	"errors"
	"sync"
	"time"

	"smokeping-modern/internal/scheduler"
)

// ErrRollupUnavailable is returned by a Rollupper whose downsampled aggregate has
// not been created (e.g. a PostgreSQL store started without -downsample, so the
// samples_hourly view is missing). The API turns it into a 501 so the UI disables
// hourly mode, rather than a 503 that looks like a transient failure.
var ErrRollupUnavailable = errors.New("store: rollup aggregate not available")

// Store is the sink the collector writes each round's outcomes to, and the API
// reads series back from.
type Store interface {
	Add(outcomes []scheduler.Outcome)
	Keys() []string
	Latest(key string) (scheduler.Outcome, bool)
	History(key string) []scheduler.Outcome
}

// RollupPoint is one downsampled bucket for a target (hourly or daily). Median
// values are NaN for buckets that were entirely lost.
type RollupPoint struct {
	Bucket    time.Time
	MedianAvg float64
	MedianMin float64
	MedianMax float64
	LossFrac  float64
	Rounds    int
}

// Rollupper is implemented by stores that support downsampled reads (the hourly
// and daily continuous aggregates). resolution selects the tier: "1h" (default)
// or "1d" — the daily tier feeds the long-range (400d) drill-down, where the
// hourly tier would be too many buckets. The API exposes it at /api/rollup.
type Rollupper interface {
	Rollup(ctx context.Context, target, resolution string) ([]RollupPoint, error)
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
	Availability(ctx context.Context, target string, cutoff time.Time, maxLossPct *float64) (AvailabilityStat, error)
}

// RangeHistorier is implemented by stores that can return a target's rounds over
// an arbitrary window (at or after cutoff), unbounded by the History cap — so a
// long drill-down range (e.g. 30h of per-round samples) isn't silently truncated
// to the last N stored rounds. Returned oldest->newest, like History. /api/series
// prefers this when given a window, falling back to the capped History otherwise.
type RangeHistorier interface {
	HistorySince(ctx context.Context, target string, cutoff time.Time) ([]scheduler.Outcome, error)
}

// LatestAller returns every target's most recent outcome in one call. The live
// endpoints (targets, charts, metrics) prefer it over one Latest per target, so a
// refresh is a single query instead of N (the #5 query fan-out reduction).
type LatestAller interface {
	LatestAll() map[string]scheduler.Outcome
}

// AvailabilityAller aggregates availability for every target over the window in one
// call — the bulk form of Availability, so /api/sla is one query instead of N.
type AvailabilityAller interface {
	AvailabilityAll(ctx context.Context, cutoff time.Time, maxLossPct *float64) (map[string]AvailabilityStat, error)
}

// MemStore is the in-memory implementation: latest + bounded history per target.
// Used by default and in tests; not durable.
type MemStore struct {
	mu      sync.RWMutex
	latest  map[string]scheduler.Outcome
	history map[string][]scheduler.Outcome
	keys    []string
	cap     int
}

func NewMem(historyCap int) *MemStore {
	if historyCap <= 0 {
		historyCap = 512
	}
	return &MemStore{
		latest:  map[string]scheduler.Outcome{},
		history: map[string][]scheduler.Outcome{},
		cap:     historyCap,
	}
}

func (s *MemStore) Add(outcomes []scheduler.Outcome) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, o := range outcomes {
		k := o.Target.Name
		if _, seen := s.latest[k]; !seen {
			s.keys = append(s.keys, k)
		}
		s.latest[k] = o
		h := append(s.history[k], o)
		if len(h) > s.cap {
			h = h[len(h)-s.cap:]
		}
		s.history[k] = h
	}
}

func (s *MemStore) Keys() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, len(s.keys))
	copy(out, s.keys)
	return out
}

func (s *MemStore) Latest(key string) (scheduler.Outcome, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	o, ok := s.latest[key]
	return o, ok
}

func (s *MemStore) History(key string) []scheduler.Outcome {
	s.mu.RLock()
	defer s.mu.RUnlock()
	h := s.history[key]
	out := make([]scheduler.Outcome, len(h))
	copy(out, h)
	return out
}

// Availability scans the in-memory history for target and aggregates the rounds at
// or after cutoff. Unlike a database store it can only see what it currently holds
// (bounded by the history cap) — best-effort, honest coverage; production uses the
// pgstore implementation, which is unbounded.
func (s *MemStore) Availability(_ context.Context, target string, cutoff time.Time, maxLossPct *float64) (AvailabilityStat, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var st AvailabilityStat
	for _, o := range s.history[target] {
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
func (s *MemStore) HistorySince(_ context.Context, target string, cutoff time.Time) ([]scheduler.Outcome, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	h := s.history[target]
	out := make([]scheduler.Outcome, 0, len(h))
	for _, o := range h {
		if o.When.Before(cutoff) {
			continue
		}
		out = append(out, o)
	}
	return out, nil
}

// LatestAll returns a copy of every target's most recent outcome.
func (s *MemStore) LatestAll() map[string]scheduler.Outcome {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]scheduler.Outcome, len(s.latest))
	for k, o := range s.latest {
		out[k] = o
	}
	return out
}

// AvailabilityAll aggregates every target over [cutoff, now) — the same best-effort
// scan as Availability, once per target it holds.
func (s *MemStore) AvailabilityAll(ctx context.Context, cutoff time.Time, maxLossPct *float64) (map[string]AvailabilityStat, error) {
	s.mu.RLock()
	keys := make([]string, len(s.keys))
	copy(keys, s.keys)
	s.mu.RUnlock()
	out := make(map[string]AvailabilityStat, len(keys))
	for _, k := range keys {
		st, err := s.Availability(ctx, k, cutoff, maxLossPct)
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
	_ Availabler        = (*MemStore)(nil)
	_ RangeHistorier    = (*MemStore)(nil)
	_ LatestAller       = (*MemStore)(nil)
	_ AvailabilityAller = (*MemStore)(nil)
)
