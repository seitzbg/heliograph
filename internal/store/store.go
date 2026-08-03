// Package store is a minimal in-memory time-series sink for the MVP. It keeps
// the latest outcome plus a bounded history per target key. In production this
// is replaced by TimescaleDB (raw N samples per round; smoke bands computed as
// SQL quantiles at query time) — see codemap 07 §4.
package store

import (
	"sync"

	"smokeping-modern/internal/scheduler"
)

type Store struct {
	mu      sync.RWMutex
	latest  map[string]scheduler.Outcome
	history map[string][]scheduler.Outcome
	keys    []string
	cap     int
}

func New(historyCap int) *Store {
	if historyCap <= 0 {
		historyCap = 512
	}
	return &Store{
		latest:  map[string]scheduler.Outcome{},
		history: map[string][]scheduler.Outcome{},
		cap:     historyCap,
	}
}

// Add records a round's outcomes.
func (s *Store) Add(outcomes []scheduler.Outcome) {
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

// Keys returns target keys in first-seen order.
func (s *Store) Keys() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, len(s.keys))
	copy(out, s.keys)
	return out
}

func (s *Store) Latest(key string) (scheduler.Outcome, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	o, ok := s.latest[key]
	return o, ok
}

func (s *Store) History(key string) []scheduler.Outcome {
	s.mu.RLock()
	defer s.mu.RUnlock()
	h := s.history[key]
	out := make([]scheduler.Outcome, len(h))
	copy(out, h)
	return out
}
