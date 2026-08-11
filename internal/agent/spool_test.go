package agent

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"smokeping-modern/internal/agentwire"
)

func TestFrameRoundTrip(t *testing.T) {
	body := []byte(`{"target":"a","ts":"2026-01-01T00:00:00Z"}`)
	buf := encodeFrame(nil, 42, body)
	seq, got, n, err := decodeFrame(buf)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if seq != 42 {
		t.Errorf("seq = %d, want 42", seq)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("body = %q, want %q", got, body)
	}
	if n != len(buf) {
		t.Errorf("n = %d, want %d", n, len(buf))
	}
}

func TestFrameTruncatedIsShort(t *testing.T) {
	buf := encodeFrame(nil, 1, []byte("hello"))
	// Any prefix shorter than the whole frame must report errShortFrame with n==0.
	for cut := 0; cut < len(buf); cut++ {
		if _, _, n, err := decodeFrame(buf[:cut]); !errors.Is(err, errShortFrame) || n != 0 {
			t.Fatalf("cut=%d: err=%v n=%d, want errShortFrame n=0", cut, err, n)
		}
	}
}

func TestFrameCorruptCRC(t *testing.T) {
	buf := encodeFrame(nil, 1, []byte("hello"))
	buf[len(buf)-1] ^= 0xFF // flip a payload byte
	if _, _, _, err := decodeFrame(buf); !errors.Is(err, errBadFrame) {
		t.Fatalf("err = %v, want errBadFrame", err)
	}
}

func TestFrameConcatenation(t *testing.T) {
	var buf []byte
	buf = encodeFrame(buf, 1, []byte("one"))
	buf = encodeFrame(buf, 2, []byte("two"))
	seq1, b1, n1, err := decodeFrame(buf)
	if err != nil || seq1 != 1 || string(b1) != "one" {
		t.Fatalf("frame1: seq=%d body=%q err=%v", seq1, b1, err)
	}
	seq2, b2, _, err := decodeFrame(buf[n1:])
	if err != nil || seq2 != 2 || string(b2) != "two" {
		t.Fatalf("frame2: seq=%d body=%q err=%v", seq2, b2, err)
	}
}

func writeRaw(t *testing.T, path string, frames ...[]byte) {
	t.Helper()
	var buf []byte
	for _, f := range frames {
		buf = append(buf, f...)
	}
	if err := os.WriteFile(path, buf, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestReadSegmentAllGood(t *testing.T) {
	p := filepath.Join(t.TempDir(), "seg.log")
	writeRaw(t, p, encodeFrame(nil, 1, []byte("a")), encodeFrame(nil, 2, []byte("b")))
	recs, err := readSegment(p, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 || recs[0].seq != 1 || recs[1].seq != 2 {
		t.Fatalf("recs = %+v", recs)
	}
	if string(recs[0].body) != "a" || string(recs[1].body) != "b" {
		t.Fatalf("bodies = %q %q", recs[0].body, recs[1].body)
	}
}

func TestReadSegmentTornTailTruncates(t *testing.T) {
	p := filepath.Join(t.TempDir(), "seg.log")
	good := encodeFrame(nil, 1, []byte("a"))
	torn := encodeFrame(nil, 2, []byte("bbbb"))[:5] // partial second frame
	writeRaw(t, p, good, torn)
	recs, err := readSegment(p, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 || recs[0].seq != 1 {
		t.Fatalf("recs = %+v, want just seq 1", recs)
	}
	fi, _ := os.Stat(p)
	if fi.Size() != int64(len(good)) {
		t.Fatalf("file size = %d, want %d (truncated to last good frame)", fi.Size(), len(good))
	}
}

func TestReadSegmentCorruptStopsThere(t *testing.T) {
	p := filepath.Join(t.TempDir(), "seg.log")
	f1 := encodeFrame(nil, 1, []byte("a"))
	f2 := encodeFrame(nil, 2, []byte("b"))
	f2[len(f2)-1] ^= 0xFF // corrupt second frame's payload
	f3 := encodeFrame(nil, 3, []byte("c"))
	writeRaw(t, p, f1, f2, f3)
	recs, err := readSegment(p, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 || recs[0].seq != 1 {
		t.Fatalf("recs = %+v, want to stop at corruption after seq 1", recs)
	}
}

func TestReadSegmentMissingFileIsError(t *testing.T) {
	if _, err := readSegment(filepath.Join(t.TempDir(), "nope.log"), false); err == nil {
		t.Fatal("want error for missing file")
	}
}

func TestHeadFileRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if h, err := readHead(dir); err != nil || h != 0 {
		t.Fatalf("absent head: h=%d err=%v, want 0,nil", h, err)
	}
	if err := writeHead(dir, 123); err != nil {
		t.Fatal(err)
	}
	if h, err := readHead(dir); err != nil || h != 123 {
		t.Fatalf("h=%d err=%v, want 123", h, err)
	}
	if err := writeHead(dir, 456); err != nil {
		t.Fatal(err)
	}
	if h, _ := readHead(dir); h != 456 {
		t.Fatalf("h=%d, want 456 after rewrite", h)
	}
}

func TestSpoolAppendFlushReplay(t *testing.T) {
	dir := t.TempDir()
	sp, head, live, err := openSpool(dir)
	if err != nil {
		t.Fatal(err)
	}
	if head != 0 || len(live) != 0 {
		t.Fatalf("fresh spool: head=%d live=%d, want 0,0", head, len(live))
	}
	sp.append(0, rr("a"))
	sp.append(1, rr("b"))
	if err := sp.flush(); err != nil {
		t.Fatal(err)
	}
	if err := sp.close(); err != nil {
		t.Fatal(err)
	}

	// reopen == crash recovery
	sp2, head2, live2, err := openSpool(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer sp2.close()
	if head2 != 0 {
		t.Fatalf("head2 = %d, want 0", head2)
	}
	if len(live2) != 2 || live2[0].Target != "a" || live2[1].Target != "b" {
		t.Fatalf("live2 = %+v, want [a b]", live2)
	}
	if sp2.replayed() != 2 {
		t.Fatalf("replayed() = %d, want 2", sp2.replayed())
	}
}

func TestSpoolHeadExcludesDead(t *testing.T) {
	dir := t.TempDir()
	sp, _, _, _ := openSpool(dir)
	sp.append(0, rr("a"))
	sp.append(1, rr("b"))
	sp.append(2, rr("c"))
	sp.advanceHead(2) // seq 0,1 now dead; seq 2 live
	if err := sp.flush(); err != nil {
		t.Fatal(err)
	}
	sp.close()

	sp2, head2, live2, _ := openSpool(dir)
	defer sp2.close()
	if head2 != 2 {
		t.Fatalf("head2 = %d, want 2", head2)
	}
	if len(live2) != 1 || live2[0].Target != "c" {
		t.Fatalf("live2 = %+v, want [c]", live2)
	}
}

func TestSpoolSegmentRoll(t *testing.T) {
	dir := t.TempDir()
	sp, _, _, _ := openSpool(dir)
	// Force several rolls with a small cap override.
	sp.segMax = 512
	big := agentwire.RoundReport{Target: "x", TS: "2026-01-01T00:00:00Z", RTTs: make([]float64, 20)}
	for i := 0; i < 50; i++ {
		sp.append(int64(i), big)
	}
	if err := sp.flush(); err != nil {
		t.Fatal(err)
	}
	sp.close()

	segs, _ := filepath.Glob(filepath.Join(dir, "seg-*.log"))
	if len(segs) < 2 {
		t.Fatalf("got %d segments, want >= 2 (rolls)", len(segs))
	}
	sp2, _, live2, _ := openSpool(dir)
	defer sp2.close()
	if len(live2) != 50 {
		t.Fatalf("live2 = %d, want 50 across segments", len(live2))
	}
}

// TestSpoolReclaimDeletesSegmentFile exercises reclaimLocked's os.Remove path: once the head
// advances past a closed segment's max seq, that segment must be both reported via reclaimed()
// and actually removed from disk, while the active segment (never reclaimed) survives.
func TestSpoolReclaimDeletesSegmentFile(t *testing.T) {
	dir := t.TempDir()
	sp, _, _, err := openSpool(dir)
	if err != nil {
		t.Fatal(err)
	}
	sp.segMax = 512
	big := agentwire.RoundReport{Target: "x", TS: "2026-01-01T00:00:00Z", RTTs: make([]float64, 20)}
	for i := 0; i < 50; i++ {
		sp.append(int64(i), big)
	}
	if err := sp.flush(); err != nil {
		t.Fatal(err)
	}

	segsBefore, _ := filepath.Glob(filepath.Join(dir, "seg-*.log"))
	if len(segsBefore) < 2 {
		t.Fatalf("got %d segments before reclaim, want >= 2 (rolls)", len(segsBefore))
	}
	if len(sp.segments) == 0 {
		t.Fatal("expected at least one closed (non-active) segment to reclaim")
	}
	firstClosedPath := sp.segments[0].path
	firstClosedMax := sp.segments[0].maxSeq

	sp.advanceHead(firstClosedMax + 1) // every record in the first closed segment is now dead
	if err := sp.flush(); err != nil {
		t.Fatal(err)
	}

	if sp.reclaimed() == 0 {
		t.Fatalf("reclaimed() = 0, want > 0")
	}
	if _, err := os.Stat(firstClosedPath); !os.IsNotExist(err) {
		t.Fatalf("first closed segment %s should have been deleted, stat err=%v", firstClosedPath, err)
	}
	segsAfter, _ := filepath.Glob(filepath.Join(dir, "seg-*.log"))
	if len(segsAfter) == 0 {
		t.Fatal("active segment should still be present after reclaim")
	}

	if err := sp.close(); err != nil {
		t.Fatal(err)
	}

	sp2, head2, live2, err := openSpool(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer sp2.close()
	if head2 != firstClosedMax+1 {
		t.Fatalf("head2 = %d, want %d", head2, firstClosedMax+1)
	}
	wantLive := 50 - int(firstClosedMax+1)
	if len(live2) != wantLive {
		t.Fatalf("live2 = %d, want %d (seqs %d..49 alive)", len(live2), wantLive, firstClosedMax+1)
	}
}

func TestSpoolFlockRejectsSecondOpener(t *testing.T) {
	dir := t.TempDir()
	sp1, _, _, err := openSpool(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer sp1.close()
	if _, _, _, err := openSpool(dir); err == nil {
		t.Fatal("second openSpool on the same dir must fail (lock contended)")
	}
	// After close, a new opener succeeds.
	sp1.close()
	sp3, _, _, err := openSpool(dir)
	if err != nil {
		t.Fatalf("reopen after close: %v", err)
	}
	sp3.close()
}

func TestSpoolDegradesOnWriteError(t *testing.T) {
	dir := t.TempDir()
	sp, _, _, _ := openSpool(dir)
	defer sp.close()
	sp.writeErr = errors.New("disk full")
	sp.append(0, rr("a"))
	if err := sp.flush(); err == nil {
		t.Fatal("flush should surface the injected write error")
	}
	if sp.errors() == 0 {
		t.Fatal("error count not incremented")
	}
	// Degraded: further appends are no-ops and must not panic or re-error.
	sp.append(1, rr("b"))
	if err := sp.flush(); err != nil {
		t.Fatalf("degraded flush should be a no-op, got %v", err)
	}
}

// TestSpoolStartCloseLifecycleIsIdempotent covers the background flusher goroutine
// (start/flushLoop) end to end, and is the regression test for the close()-after-start()
// double-close panic: a second close() must be a safe, error-free no-op.
func TestSpoolStartCloseLifecycleIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	sp, _, _, err := openSpool(dir)
	if err != nil {
		t.Fatal(err)
	}
	sp.start()
	sp.append(0, rr("a"))
	if err := sp.close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	// Regression for the close()-after-start() double-close-of-s.stop panic: calling close()
	// again must not panic, and must return nil without re-flushing or re-closing anything.
	if err := sp.close(); err != nil {
		t.Fatalf("second close() = %v, want nil", err)
	}

	sp2, head2, live2, err := openSpool(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer sp2.close()
	if head2 != 0 {
		t.Fatalf("head2 = %d, want 0", head2)
	}
	if len(live2) != 1 || live2[0].Target != "a" {
		t.Fatalf("live2 = %+v, want [a]", live2)
	}
}

func TestBufferSpoolCrashReplay(t *testing.T) {
	dir := t.TempDir()

	// Run 1: buffer with a spool. Add 3 rounds, flush (fsync), commit the first, flush again.
	sp, head, live, err := openSpool(dir)
	if err != nil {
		t.Fatal(err)
	}
	b := newBuffer(100)
	b.setPersister(sp)
	b.reload(head, live)
	b.add(rr("a")) // seq 0
	b.add(rr("b")) // seq 1
	b.add(rr("c")) // seq 2
	if err := sp.flush(); err != nil {
		t.Fatal(err)
	}
	batch, upto := b.peekBatch(1, 1<<30) // just seq 0
	if len(batch) != 1 || batch[0].Target != "a" {
		t.Fatalf("batch = %+v", batch)
	}
	b.commit(upto)                     // headSeq -> 1
	if err := sp.flush(); err != nil { // persist the advanced watermark
		t.Fatal(err)
	}
	// simulate a hard crash: the kernel reclaims fds (releasing the flock) but no
	// graceful flush/close runs — on-disk state is exactly what the last flush() fsynced.
	if err := syscall.Flock(int(sp.lockFile.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	sp.lockFile.Close()
	sp = nil
	b = nil

	// Run 2: reopen; expect b and c live, a gone.
	sp2, head2, live2, err := openSpool(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer sp2.close()
	b2 := newBuffer(100)
	b2.setPersister(sp2)
	b2.reload(head2, live2)
	if b2.len() != 2 {
		t.Fatalf("recovered len = %d, want 2 (b,c)", b2.len())
	}
	got, _ := b2.peekBatch(10, 1<<30)
	if got[0].Target != "b" || got[1].Target != "c" {
		t.Fatalf("recovered = %+v, want [b c]", got)
	}
}

func TestBufferSpoolCrashBeforeWatermarkReplaysDuplicate(t *testing.T) {
	dir := t.TempDir()
	sp, head, live, _ := openSpool(dir)
	b := newBuffer(100)
	b.setPersister(sp)
	b.reload(head, live)
	b.add(rr("a"))
	b.add(rr("b"))
	sp.flush() // data persisted
	_, upto := b.peekBatch(2, 1<<30)
	b.commit(upto) // headSeq advances in memory, but we crash before the next flush
	// no flush here -> watermark NOT persisted
	// simulate a hard crash: the kernel reclaims fds (releasing the flock) but no
	// graceful flush/close runs — on-disk state is exactly what the last flush() fsynced.
	if err := syscall.Flock(int(sp.lockFile.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	sp.lockFile.Close()
	sp = nil

	sp2, head2, live2, _ := openSpool(dir)
	defer sp2.close()
	// Bounded duplication: both rounds replay because the watermark wasn't fsynced.
	// This is safe — the hub dedups on (target,vantage,ts).
	if len(live2) != 2 || head2 != 0 {
		t.Fatalf("live2=%d head2=%d, want 2 rounds re-presented (bounded dup)", len(live2), head2)
	}
}
