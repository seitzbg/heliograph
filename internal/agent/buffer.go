package agent

import (
	"sync"
	"sync/atomic"

	"smokeping-modern/internal/agentwire"
)

// buffer is a bounded FIFO of pending rounds. The measure loop add()s; the flush loop
// peekBatch()es and commit()s on a successful push. When full, add drops the oldest
// (and counts it) rather than blocking the measure loop — bounded memory, never silent.
type buffer struct {
	mu      sync.Mutex
	rounds  []agentwire.RoundReport
	cap     int
	dropCnt atomic.Int64
}

func newBuffer(capRounds int) *buffer {
	if capRounds <= 0 {
		capRounds = 100_000
	}
	return &buffer{cap: capRounds}
}

func (b *buffer) add(r agentwire.RoundReport) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.rounds) >= b.cap {
		b.rounds = b.rounds[1:] // drop oldest
		b.dropCnt.Add(1)
	}
	b.rounds = append(b.rounds, r)
}

func (b *buffer) peekBatch(max int) []agentwire.RoundReport {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(b.rounds)
	if n > max {
		n = max
	}
	out := make([]agentwire.RoundReport, n)
	copy(out, b.rounds[:n])
	return out
}

// commit removes the n oldest rounds after a successful push. n is clamped to the
// current length. Re-slicing b.rounds[n:] (and add's [1:]) retains the backing array's
// head over time rather than reallocating; that's acceptable for a bounded buffer since
// the array is capped at ~cap entries either way.
func (b *buffer) commit(n int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if n > len(b.rounds) {
		n = len(b.rounds)
	}
	b.rounds = b.rounds[n:]
}

func (b *buffer) len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.rounds)
}

func (b *buffer) dropped() int64 { return b.dropCnt.Load() }
