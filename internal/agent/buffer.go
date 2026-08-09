package agent

import (
	"sync"
	"sync/atomic"

	"smokeping-modern/internal/agentwire"
)

// buffer is a bounded FIFO of pending rounds. The measure loop add()s; the flush loop
// peekBatch()es and commit()s on a successful push. When full, add drops the oldest
// (and counts it) rather than blocking the measure loop — bounded memory, never silent.
//
// commit is keyed by sequence number rather than count: peekBatch hands back the seq
// of the last round in the batch, and commit removes only rounds up to that seq. This
// keeps a peek -> push -> commit cycle correct even when a concurrent add() evicts the
// oldest round(s) in between — a plain "commit(n oldest)" would otherwise remove
// whatever happens to be at the front at commit time, which after a concurrent
// eviction is no longer the batch that was actually pushed (silently dropping an
// un-pushed, never-counted round). See TestBufferCommitSurvivesConcurrentEviction.
type buffer struct {
	mu      sync.Mutex
	rounds  []agentwire.RoundReport
	headSeq int64 // sequence number of rounds[0]; advances on evict and commit
	cap     int
	dropCnt atomic.Int64
	rejCnt  atomic.Int64 // rounds discarded because the hub permanently rejected their batch
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
		b.headSeq++
		b.dropCnt.Add(1)
	}
	b.rounds = append(b.rounds, r)
}

// peekBatch returns a copy of up to max oldest rounds, without removing them, plus
// upto: the sequence number of the last round in the returned batch. Pass upto to
// commit after a successful push of the batch. When the buffer is empty, upto is
// headSeq-1 so a subsequent commit is a no-op.
func (b *buffer) peekBatch(max int) ([]agentwire.RoundReport, int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(b.rounds)
	if n > max {
		n = max
	}
	out := make([]agentwire.RoundReport, n)
	copy(out, b.rounds[:n])
	return out, b.headSeq + int64(n) - 1
}

// commit removes all still-present rounds with sequence number <= upto (i.e. the
// batch a prior peekBatch returned, after it was pushed successfully). Rounds already
// evicted by a concurrent add() are skipped rather than over-removed, so commit never
// discards rounds newer than the pushed batch.
//
// Residual (acceptable) edge case: a round that is peeked, then evicted by a
// concurrent full add() before commit runs, but that was in fact delivered by the
// in-flight push, is still counted once as dropped even though it was delivered —
// a metric-accuracy blip, not data loss (the hub dedups incoming rounds on
// (target, vantage, ts)).
func (b *buffer) commit(upto int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	remove := upto - b.headSeq + 1
	if remove <= 0 {
		return
	}
	if remove > int64(len(b.rounds)) {
		remove = int64(len(b.rounds))
	}
	// Re-slicing (here and in add's [1:]) retains the backing array's head over time
	// rather than reallocating; acceptable for a bounded buffer since the array is
	// capped at ~cap entries either way.
	b.rounds = b.rounds[remove:]
	b.headSeq += remove
}

func (b *buffer) len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.rounds)
}

func (b *buffer) dropped() int64 { return b.dropCnt.Load() }

// reject counts rounds discarded because the hub permanently rejected their batch (distinct
// from dropCnt, which counts oldest rounds evicted when the buffer overflows).
func (b *buffer) reject(n int)    { b.rejCnt.Add(int64(n)) }
func (b *buffer) rejected() int64 { return b.rejCnt.Load() }
