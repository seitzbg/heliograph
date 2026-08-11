package agent

import (
	"testing"

	"smokeping-modern/internal/agentwire"
)

func rr(name string) agentwire.RoundReport { return agentwire.RoundReport{Target: name} }

// rrN builds a round with n RTT values — enough to exercise byte-based bounds without
// allocating an OOM-scale fixture (CODE_REVIEW #5).
func rrN(n int) agentwire.RoundReport {
	return agentwire.RoundReport{Target: "t", RTTs: make([]float64, n)}
}

func TestEstimatedJSONBytesGrowsWithRTTs(t *testing.T) {
	if estimatedJSONBytes(rrN(100)) <= estimatedJSONBytes(rrN(1)) {
		t.Fatal("byte estimate must grow with the number of RTTs")
	}
}

// peekBatch must cap a batch by estimated bytes, not just round count, so the agent never
// marshals a body over the hub's cap and OOMs before the 413 split can run (CODE_REVIEW #5).
// It must still return at least one round even when that round alone exceeds the budget.
func TestPeekBatchByteBudget(t *testing.T) {
	b := newBuffer(100) // round cap high, so the BYTE budget is what limits the batch
	for i := 0; i < 10; i++ {
		b.add(rrN(100))
	}
	budget := 3 * estimatedJSONBytes(rrN(100))
	batch, _ := b.peekBatch(100, budget)
	if len(batch) != 3 {
		t.Fatalf("byte-capped peek took %d rounds, want 3 (budget=%d)", len(batch), budget)
	}
	if one, _ := b.peekBatch(100, 1); len(one) != 1 {
		t.Fatalf("peek must always return >=1 round even under a tiny byte budget, got %d", len(one))
	}
}

// A prolonged outage of large rounds must not grow the buffer past its byte budget: add evicts
// oldest by bytes, independent of the round cap (CODE_REVIEW #5).
func TestBufferEvictsByBytes(t *testing.T) {
	b := newBuffer(1000) // high round cap so the byte budget is what bites
	b.maxBytes = 4 * estimatedJSONBytes(rrN(100))
	for i := 0; i < 20; i++ {
		b.add(rrN(100))
	}
	if b.len() > 4 {
		t.Fatalf("buffer held %d rounds, want <=4 (byte budget)", b.len())
	}
	if b.dropped() == 0 {
		t.Fatal("byte-budget eviction should have counted drops")
	}
}

func TestBufferDropOldestWhenFull(t *testing.T) {
	b := newBuffer(2)
	b.add(rr("a"))
	b.add(rr("b"))
	b.add(rr("c")) // evicts "a"
	if b.len() != 2 || b.dropped() != 1 {
		t.Fatalf("len=%d dropped=%d", b.len(), b.dropped())
	}
	got, _ := b.peekBatch(10, 1<<30)
	if len(got) != 2 || got[0].Target != "b" || got[1].Target != "c" {
		t.Fatalf("peek=%+v", got)
	}
}

func TestBufferPeekCommit(t *testing.T) {
	b := newBuffer(10)
	for _, n := range []string{"a", "b", "c"} {
		b.add(rr(n))
	}
	batch, upto := b.peekBatch(2, 1<<30)
	if len(batch) != 2 || b.len() != 3 { // peek does not remove
		t.Fatalf("peek batch=%d len=%d", len(batch), b.len())
	}
	b.commit(upto) // simulate successful push of the 2 oldest
	rest, _ := b.peekBatch(10, 1<<30)
	if b.len() != 1 || rest[0].Target != "c" {
		t.Fatalf("after commit len=%d", b.len())
	}
}

// TestBufferCommitSurvivesConcurrentEviction reproduces the interleaving from the
// review finding: a peek is taken, then a concurrent add() fills the buffer and
// evicts the oldest round(s) before commit runs. commit(upto) must remove only the
// rounds that were actually part of the peeked-and-pushed batch (b, c) — it must NOT
// remove d, which was added after the peek and never pushed.
func TestBufferCommitSurvivesConcurrentEviction(t *testing.T) {
	b := newBuffer(5)
	for _, n := range []string{"a", "b", "c", "d", "e"} {
		b.add(rr(n))
	}
	batch, upto := b.peekBatch(3, 1<<30) // [a,b,c], upto = seq of c
	if len(batch) != 3 {
		t.Fatalf("batch=%d", len(batch))
	}
	b.add(rr("f")) // full: evicts a, appends f -> [b,c,d,e,f]
	b.commit(upto) // must remove only the still-present pushed rounds (b,c), NOT d

	remaining, _ := b.peekBatch(10, 1<<30)
	names := make([]string, len(remaining))
	for i, r := range remaining {
		names[i] = r.Target
	}
	if len(names) != 3 || names[0] != "d" || names[1] != "e" || names[2] != "f" {
		t.Fatalf("after evict+commit want [d e f], got %v (d must not be lost)", names)
	}
}
