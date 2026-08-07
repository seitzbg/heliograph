package agent

import (
	"testing"

	"smokeping-modern/internal/agentwire"
)

func rr(name string) agentwire.RoundReport { return agentwire.RoundReport{Target: name} }

func TestBufferDropOldestWhenFull(t *testing.T) {
	b := newBuffer(2)
	b.add(rr("a"))
	b.add(rr("b"))
	b.add(rr("c")) // evicts "a"
	if b.len() != 2 || b.dropped() != 1 {
		t.Fatalf("len=%d dropped=%d", b.len(), b.dropped())
	}
	got, _ := b.peekBatch(10)
	if len(got) != 2 || got[0].Target != "b" || got[1].Target != "c" {
		t.Fatalf("peek=%+v", got)
	}
}

func TestBufferPeekCommit(t *testing.T) {
	b := newBuffer(10)
	for _, n := range []string{"a", "b", "c"} {
		b.add(rr(n))
	}
	batch, upto := b.peekBatch(2)
	if len(batch) != 2 || b.len() != 3 { // peek does not remove
		t.Fatalf("peek batch=%d len=%d", len(batch), b.len())
	}
	b.commit(upto) // simulate successful push of the 2 oldest
	rest, _ := b.peekBatch(10)
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
	batch, upto := b.peekBatch(3) // [a,b,c], upto = seq of c
	if len(batch) != 3 {
		t.Fatalf("batch=%d", len(batch))
	}
	b.add(rr("f")) // full: evicts a, appends f -> [b,c,d,e,f]
	b.commit(upto) // must remove only the still-present pushed rounds (b,c), NOT d

	remaining, _ := b.peekBatch(10)
	names := make([]string, len(remaining))
	for i, r := range remaining {
		names[i] = r.Target
	}
	if len(names) != 3 || names[0] != "d" || names[1] != "e" || names[2] != "f" {
		t.Fatalf("after evict+commit want [d e f], got %v (d must not be lost)", names)
	}
}
