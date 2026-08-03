// Package store defines the time-series sink interface and an in-memory
// implementation. The persistent implementation lives in store/pgstore
// (TimescaleDB). Keeping raw per-round samples is deliberate: the smoke bands
// are computed from the distribution, not just the median (see codemap 07 §4).
package store

import (
	"sync"

	"smokeping-modern/internal/scheduler"
)

// Store is the sink the collector writes each round's outcomes to, and the API
// reads series back from.
type Store interface {
	Add(outcomes []scheduler.Outcome)
	Keys() []string
	Latest(key string) (scheduler.Outcome, bool)
	History(key string) []scheduler.Outcome
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

// compile-time check
var _ Store = (*MemStore)(nil)
