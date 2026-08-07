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
	got := b.peekBatch(10)
	if len(got) != 2 || got[0].Target != "b" || got[1].Target != "c" {
		t.Fatalf("peek=%+v", got)
	}
}

func TestBufferPeekCommit(t *testing.T) {
	b := newBuffer(10)
	for _, n := range []string{"a", "b", "c"} {
		b.add(rr(n))
	}
	batch := b.peekBatch(2)
	if len(batch) != 2 || b.len() != 3 { // peek does not remove
		t.Fatalf("peek batch=%d len=%d", len(batch), b.len())
	}
	b.commit(2) // simulate successful push of the 2 oldest
	if b.len() != 1 || b.peekBatch(10)[0].Target != "c" {
		t.Fatalf("after commit len=%d", b.len())
	}
}
