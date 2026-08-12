package agent

import (
	"sync"
	"sync/atomic"

	"github.com/seitzbg/heliograph/internal/agentwire"
)

// Byte accounting for the buffer and its flush batches. A round is bounded in COUNT (up to
// config.MaxPings = 10000 RTTs) but that makes its serialized size vary by orders of magnitude,
// so bounding only by round count lets a prolonged outage of high-ping targets grow the buffer
// past a practical memory bound, and lets a count-selected flush batch marshal a
// multi-hundred-MB body the hub will only 413 AFTER the agent has already built it — OOM-killing
// a constrained vantage before the recursive 413 split can ever run (CODE_REVIEW #5). Both the
// buffer and the flush batch are therefore bounded by ESTIMATED serialized bytes as well.
const (
	// rttJSONBytes over-estimates one float64 RTT's JSON size (value + comma); real values are
	// shorter, so batches land safely under the hub's byte cap and the 413 split stays a mere
	// fallback for estimation variance.
	rttJSONBytes = 24
	// roundJSONOverhead covers a round's fixed JSON keys/punctuation, the RFC3339 ts, and the
	// numeric pings/duration; the variable-length target/fingerprint/err strings are added on top.
	roundJSONOverhead = 160
	// jsonStringExpansion bounds how much `encoding/json` can inflate a string field. The client
	// marshals with default HTML escaping, so `&`,`<`,`>` and control bytes each become a 6-byte
	// \u00XX sequence. Counting the string fields (target/err/fingerprint — none length- or
	// character-bounded) at this worst case keeps a packed batch's real encoded size under the
	// hub's cap even for adversarial content, so the agent never marshals a body the hub will 413
	// only after building it (CODE_REVIEW: estimator undercount).
	jsonStringExpansion = 6
	// defaultBufferBytes bounds total buffered memory independent of the round cap. Generous, so
	// it only bites pathological (very high pings) configs; ordinary small rounds hit the round
	// cap first.
	defaultBufferBytes = 256 << 20 // 256 MiB
	// flushByteBudget is the per-request byte budget peekBatch packs under: the hub's cap less a
	// margin for the `{"results":[…]}` envelope and estimator variance, so a packed batch's real
	// marshaled body stays below agentwire.MaxResultsBytes.
	flushByteBudget = agentwire.MaxResultsBytes - (64 << 10)
)

// estimatedJSONBytes is a conservative upper estimate of a round's serialized size: string fields
// are counted at their worst-case JSON-escaped width and RTTs at a padded per-value width, so it
// never underestimates the real encoded size for adversarial content. It is used to pack a flush
// batch under flushByteBudget before marshaling and to bound total buffer memory.
func estimatedJSONBytes(r agentwire.RoundReport) int {
	strBytes := jsonStringExpansion * (len(r.Target) + len(r.Fingerprint) + len(r.Err))
	return roundJSONOverhead + strBytes + len(r.RTTs)*rttJSONBytes
}

// buffer is a bounded FIFO of pending rounds. The measure loop add()s; the flush loop
// peekBatch()es and commit()s on a successful push. When full (by round count OR estimated
// bytes), add drops the oldest (and counts it) rather than blocking the measure loop — bounded
// memory, never silent.
//
// commit is keyed by sequence number rather than count: peekBatch hands back the seq
// of the last round in the batch, and commit removes only rounds up to that seq. This
// keeps a peek -> push -> commit cycle correct even when a concurrent add() evicts the
// oldest round(s) in between — a plain "commit(n oldest)" would otherwise remove
// whatever happens to be at the front at commit time, which after a concurrent
// eviction is no longer the batch that was actually pushed (silently dropping an
// un-pushed, never-counted round). See TestBufferCommitSurvivesConcurrentEviction.
type buffer struct {
	mu       sync.Mutex
	rounds   []agentwire.RoundReport
	headSeq  int64 // sequence number of rounds[0]; advances on evict and commit
	cap      int
	maxBytes int // byte budget for total buffered memory
	curBytes int // running sum of estimatedJSONBytes(rounds[i])
	dropCnt  atomic.Int64
	rejCnt   atomic.Int64 // rounds discarded because the hub permanently rejected their batch
	p        persister    // optional durable mirror of the live set; nil = in-memory only
}

// persister durably mirrors the buffer's live set. nil = in-memory only.
type persister interface {
	append(seq int64, r agentwire.RoundReport)
	advanceHead(headSeq int64)
}

func newBuffer(capRounds int) *buffer {
	if capRounds <= 0 {
		capRounds = 100_000
	}
	return &buffer{cap: capRounds, maxBytes: defaultBufferBytes}
}

// setPersister attaches (or clears, with nil) the buffer's durable mirror. Must be called
// before the measure/flush loops start using the buffer to avoid a torn view of history.
func (b *buffer) setPersister(p persister) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.p = p
}

// evictToFitLocked evicts oldest rounds until sz fits within BOTH the round cap and the byte
// budget, advancing headSeq/dropCnt/curBytes as it goes. Never evicts below one round: a lone
// round bigger than the whole budget is kept and later handled by the flush 413-split/drop path
// (CODE_REVIEW #5). Callers must hold b.mu. It does NOT itself notify the persister — callers
// that evicted (return value) are responsible for calling b.p.advanceHead(b.headSeq) once,
// after this returns, so a single flush() call is used per add/reload step rather than one per
// evicted round.
func (b *buffer) evictToFitLocked(sz int) (evicted bool) {
	for len(b.rounds) > 0 && (len(b.rounds) >= b.cap || b.curBytes+sz > b.maxBytes) {
		b.curBytes -= estimatedJSONBytes(b.rounds[0])
		b.rounds = b.rounds[1:]
		b.headSeq++
		b.dropCnt.Add(1)
		evicted = true
	}
	return evicted
}

func (b *buffer) add(r agentwire.RoundReport) {
	sz := estimatedJSONBytes(r)
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.evictToFitLocked(sz) && b.p != nil {
		b.p.advanceHead(b.headSeq)
	}
	b.rounds = append(b.rounds, r)
	b.curBytes += sz
	if b.p != nil {
		b.p.append(b.headSeq+int64(len(b.rounds))-1, r)
	}
}

// peekBatch returns a copy of the oldest rounds — up to maxRounds AND up to (but not exceeding)
// maxBytes of estimated serialized size — without removing them, plus upto: the sequence number
// of the last round in the returned batch. Pass upto to commit after a successful push. At least
// one round is always returned when the buffer is non-empty, even if that single round exceeds
// maxBytes (the flush path splits/drops it), so the loop can never stall. When the buffer is
// empty, upto is headSeq-1 so a subsequent commit is a no-op.
func (b *buffer) peekBatch(maxRounds, maxBytes int) ([]agentwire.RoundReport, int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(b.rounds)
	if n > maxRounds {
		n = maxRounds
	}
	bytes, take := 0, 0
	for take < n {
		sz := estimatedJSONBytes(b.rounds[take])
		if take > 0 && bytes+sz > maxBytes {
			break // adding this round would exceed the byte budget; stop (we already have >=1)
		}
		bytes += sz
		take++
	}
	out := make([]agentwire.RoundReport, take)
	copy(out, b.rounds[:take])
	return out, b.headSeq + int64(take) - 1
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
	for i := int64(0); i < remove; i++ {
		b.curBytes -= estimatedJSONBytes(b.rounds[i])
	}
	// Re-slicing (here and in add's [1:]) retains the backing array's head over time
	// rather than reallocating; acceptable for a bounded buffer since the array is
	// capped at ~cap entries either way.
	b.rounds = b.rounds[remove:]
	b.headSeq += remove
	if b.p != nil {
		b.p.advanceHead(b.headSeq)
	}
}

// reload seeds the buffer from a persisted head watermark and its live rounds (already on
// disk). It adopts the seq range (headSeq = head) so future appends stay consistent with disk,
// and does NOT re-persist the rounds. If the persisted set exceeds the current bounds (e.g.
// BufferCap was lowered), the oldest are evicted and the persister head is advanced so their
// disk copies can be reclaimed.
func (b *buffer) reload(head int64, rounds []agentwire.RoundReport) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.headSeq = head
	b.rounds = nil
	b.curBytes = 0
	for _, r := range rounds {
		sz := estimatedJSONBytes(r)
		if b.evictToFitLocked(sz) && b.p != nil {
			b.p.advanceHead(b.headSeq)
		}
		b.rounds = append(b.rounds, r)
		b.curBytes += sz
	}
}

func (b *buffer) len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.rounds)
}

// budget returns the buffer's count and byte bounds so spool recovery (openSpool) can apply the
// same eviction policy while decoding, rather than building an unbounded intermediate slice and
// only trimming it after the fact.
func (b *buffer) budget() (capRounds, maxBytes int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.cap, b.maxBytes
}

func (b *buffer) dropped() int64 { return b.dropCnt.Load() }

// reject counts rounds discarded because the hub permanently rejected their batch (distinct
// from dropCnt, which counts oldest rounds evicted when the buffer overflows).
func (b *buffer) reject(n int)    { b.rejCnt.Add(int64(n)) }
func (b *buffer) rejected() int64 { return b.rejCnt.Load() }
