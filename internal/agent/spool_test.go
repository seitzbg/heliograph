package agent

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
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

// openSpoolT opens a spool with generous budgets, so tests that don't specifically exercise the
// recovery bound see no eviction (the pre-CODE_REVIEW-#1 behavior).
func openSpoolT(dir string) (*spool, int64, []agentwire.RoundReport, error) {
	return openSpool(dir, 1_000_000, 1<<30)
}

func TestReadSegmentAllGood(t *testing.T) {
	p := filepath.Join(t.TempDir(), "seg.log")
	all := append(encodeFrame(nil, 1, []byte("a")), encodeFrame(nil, 2, []byte("b"))...)
	writeRaw(t, p, all)
	recs, consumed, stopErr, err := readSegment(p)
	if err != nil {
		t.Fatal(err)
	}
	if stopErr != nil || consumed != int64(len(all)) {
		t.Fatalf("stopErr=%v consumed=%d, want nil %d (clean decode)", stopErr, consumed, len(all))
	}
	if len(recs) != 2 || recs[0].seq != 1 || recs[1].seq != 2 {
		t.Fatalf("recs = %+v", recs)
	}
	if string(recs[0].body) != "a" || string(recs[1].body) != "b" {
		t.Fatalf("bodies = %q %q", recs[0].body, recs[1].body)
	}
}

// readSegment reports an incomplete decode (a torn tail) via complete=false + the consumed
// offset, but does NOT modify the file — truncation is openSpool's decision (only for the
// active segment). See TestSpoolTornLastSegmentTruncates for the truncation behavior.
func TestReadSegmentTornTailIsIncomplete(t *testing.T) {
	p := filepath.Join(t.TempDir(), "seg.log")
	good := encodeFrame(nil, 1, []byte("a"))
	torn := encodeFrame(nil, 2, []byte("bbbb"))[:5] // partial second frame
	writeRaw(t, p, good, torn)
	recs, consumed, stopErr, err := readSegment(p)
	if err != nil {
		t.Fatal(err)
	}
	if !errors.Is(stopErr, errShortFrame) || consumed != int64(len(good)) {
		t.Fatalf("stopErr=%v consumed=%d, want errShortFrame %d (torn tail, stop at last good frame)", stopErr, consumed, len(good))
	}
	if len(recs) != 1 || recs[0].seq != 1 {
		t.Fatalf("recs = %+v, want just seq 1", recs)
	}
	if fi, _ := os.Stat(p); fi.Size() != int64(len(good)+len(torn)) {
		t.Fatalf("readSegment must not modify the file; size = %d, want %d", fi.Size(), len(good)+len(torn))
	}
}

func TestReadSegmentCorruptStopsThere(t *testing.T) {
	p := filepath.Join(t.TempDir(), "seg.log")
	f1 := encodeFrame(nil, 1, []byte("a"))
	f2 := encodeFrame(nil, 2, []byte("b"))
	f2[len(f2)-1] ^= 0xFF // corrupt second frame's payload
	f3 := encodeFrame(nil, 3, []byte("c"))
	writeRaw(t, p, f1, f2, f3)
	recs, consumed, stopErr, err := readSegment(p)
	if err != nil {
		t.Fatal(err)
	}
	if !errors.Is(stopErr, errBadFrame) || consumed != int64(len(f1)) {
		t.Fatalf("stopErr=%v consumed=%d, want errBadFrame %d (checksum corruption, stop there)", stopErr, consumed, len(f1))
	}
	if len(recs) != 1 || recs[0].seq != 1 {
		t.Fatalf("recs = %+v, want to stop at corruption after seq 1", recs)
	}
}

func TestReadSegmentMissingFileIsError(t *testing.T) {
	if _, _, _, err := readSegment(filepath.Join(t.TempDir(), "nope.log")); err == nil {
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
	sp, head, live, err := openSpoolT(dir)
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
	sp2, head2, live2, err := openSpoolT(dir)
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
	sp, _, _, _ := openSpoolT(dir)
	sp.append(0, rr("a"))
	sp.append(1, rr("b"))
	sp.append(2, rr("c"))
	sp.advanceHead(2) // seq 0,1 now dead; seq 2 live
	if err := sp.flush(); err != nil {
		t.Fatal(err)
	}
	sp.close()

	sp2, head2, live2, _ := openSpoolT(dir)
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
	sp, _, _, _ := openSpoolT(dir)
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
	sp2, _, live2, _ := openSpoolT(dir)
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
	sp, _, _, err := openSpoolT(dir)
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

	sp2, head2, live2, err := openSpoolT(dir)
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
	sp1, _, _, err := openSpoolT(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer sp1.close()
	if _, _, _, err := openSpoolT(dir); err == nil {
		t.Fatal("second openSpool on the same dir must fail (lock contended)")
	}
	// After close, a new opener succeeds.
	sp1.close()
	sp3, _, _, err := openSpoolT(dir)
	if err != nil {
		t.Fatalf("reopen after close: %v", err)
	}
	sp3.close()
}

func TestSpoolDegradesOnWriteError(t *testing.T) {
	dir := t.TempDir()
	sp, _, _, _ := openSpoolT(dir)
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
	sp, _, _, err := openSpoolT(dir)
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

	sp2, head2, live2, err := openSpoolT(dir)
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
	sp, head, live, err := openSpoolT(dir)
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
	sp2, head2, live2, err := openSpoolT(dir)
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
	sp, head, live, _ := openSpoolT(dir)
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

	sp2, head2, live2, _ := openSpoolT(dir)
	defer sp2.close()
	// Bounded duplication: both rounds replay because the watermark wasn't fsynced.
	// This is safe — the hub dedups on (target,vantage,ts).
	if len(live2) != 2 || head2 != 0 {
		t.Fatalf("live2=%d head2=%d, want 2 rounds re-presented (bounded dup)", len(live2), head2)
	}
}

// Recovering a full spool must not exceed the running buffer's budget: openSpool evicts the
// oldest live rounds while decoding (rather than building an unbounded slice and trimming later),
// so a constrained vantage can't OOM on restart, and the returned head reflects the eviction
// (CODE_REVIEW #1). This is also the "lower BufferCap before restart" case.
func TestSpoolRecoveryEvictsToRoundBudget(t *testing.T) {
	dir := t.TempDir()
	sp, _, _, err := openSpoolT(dir)
	if err != nil {
		t.Fatal(err)
	}
	const n = 20
	for i := 0; i < n; i++ {
		sp.append(int64(i), rr(fmt.Sprintf("t%d", i)))
	}
	if err := sp.flush(); err != nil {
		t.Fatal(err)
	}
	if err := sp.close(); err != nil {
		t.Fatal(err)
	}

	const keep = 5 // lower round cap than what's on disk
	sp2, head2, live2, err := openSpool(dir, keep, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	defer sp2.close()
	if len(live2) != keep {
		t.Fatalf("recovered %d live rounds, want cap %d (oldest evicted during decode)", len(live2), keep)
	}
	if head2 != int64(n-keep) {
		t.Fatalf("effective head = %d, want %d (persisted head + dropped)", head2, n-keep)
	}
	if live2[0].Target != fmt.Sprintf("t%d", n-keep) || live2[keep-1].Target != fmt.Sprintf("t%d", n-1) {
		t.Fatalf("recovered the wrong window: first=%s last=%s (want the newest %d)", live2[0].Target, live2[keep-1].Target, keep)
	}
}

// The byte budget bounds recovery too (independent of the round cap): openSpool keeps only the
// newest rounds that fit maxBytes (CODE_REVIEW #1).
func TestSpoolRecoveryEvictsToByteBudget(t *testing.T) {
	dir := t.TempDir()
	sp, _, _, _ := openSpoolT(dir)
	const n = 20
	for i := 0; i < n; i++ {
		sp.append(int64(i), rrN(50))
	}
	if err := sp.flush(); err != nil {
		t.Fatal(err)
	}
	sp.close()

	budget := 4 * estimatedJSONBytes(rrN(50)) // high round cap, so the byte budget is what bites
	sp2, head2, live2, err := openSpool(dir, 1_000_000, budget)
	if err != nil {
		t.Fatal(err)
	}
	defer sp2.close()
	if len(live2) != 4 {
		t.Fatalf("recovered %d rounds, want 4 (byte budget)", len(live2))
	}
	if head2 != int64(n-len(live2)) {
		t.Fatalf("effective head = %d, want %d", head2, n-len(live2))
	}
}

// Corruption in a CLOSED (already-synced) segment is not a crash tail: openSpool must fail loudly
// rather than silently returning its valid prefix, which would drop the corrupt-onward records and
// break the buffer's contiguous-sequence adoption (seq reuse / double replay) (CODE_REVIEW #2).
// BenchmarkSpoolRecovery measures recovery of a large spool into a small live budget. After
// CODE_REVIEW L3, openSpool streams each segment frame-by-frame and unmarshals only the records it
// retains, instead of loading whole segments and copying every body — so recovery's transient
// footprint tracks the live budget, not the total records on disk. Run with -benchmem.
func BenchmarkSpoolRecovery(b *testing.B) {
	dir := b.TempDir()
	sp, _, _, err := openSpoolT(dir)
	if err != nil {
		b.Fatal(err)
	}
	const n = 5000
	for i := 0; i < n; i++ {
		sp.append(int64(i), rr(fmt.Sprintf("target-%d", i)))
	}
	if err := sp.flush(); err != nil {
		b.Fatal(err)
	}
	if err := sp.close(); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sp2, _, _, err := openSpool(dir, 50, 1<<30) // small round budget vs 5000 on disk
		if err != nil {
			b.Fatal(err)
		}
		_ = sp2.close()
	}
}

func TestSpoolClosedSegmentCorruptionFails(t *testing.T) {
	dir := t.TempDir()
	sp, _, _, _ := openSpoolT(dir)
	sp.segMax = 200 // small cap -> several segments roll, so seg[0] is closed
	for i := 0; i < 20; i++ {
		sp.append(int64(i), rr(fmt.Sprintf("t%d", i)))
	}
	if err := sp.flush(); err != nil {
		t.Fatal(err)
	}
	sp.close()

	segs, _ := filepath.Glob(filepath.Join(dir, "seg-*.log"))
	sort.Strings(segs)
	if len(segs) < 2 {
		t.Fatalf("need >= 2 segments to have a closed one, got %d", len(segs))
	}
	data, err := os.ReadFile(segs[0]) // the first (closed) segment
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)/2] ^= 0xFF // corrupt a byte mid-segment
	if err := os.WriteFile(segs[0], data, 0o644); err != nil {
		t.Fatal(err)
	}

	if _, _, _, err := openSpool(dir, 1_000_000, 1<<30); err == nil {
		t.Fatal("openSpool must fail on corruption in a closed segment, not silently drop records")
	}
}

// CODE_REVIEW M3: a checksum-CORRUPT frame in the active (last) segment is real corruption, not a
// crash-torn tail — openSpool must fail loudly (as it does for a closed segment), not silently
// truncate the bad frame and every valid frame after it. Contrast TestSpoolTornLastSegmentTruncates,
// where a genuinely SHORT (torn) tail is the expected crash artifact and is recovered.
func TestSpoolActiveSegmentCorruptionFails(t *testing.T) {
	dir := t.TempDir()
	sp, _, _, _ := openSpoolT(dir)
	for i := 0; i < 5; i++ {
		sp.append(int64(i), rr(fmt.Sprintf("t%d", i)))
	}
	if err := sp.flush(); err != nil {
		t.Fatal(err)
	}
	sp.close()

	segs, _ := filepath.Glob(filepath.Join(dir, "seg-*.log"))
	if len(segs) != 1 {
		t.Fatalf("want 1 (active) segment, got %d", len(segs))
	}
	data, err := os.ReadFile(segs[0])
	if err != nil {
		t.Fatal(err)
	}
	// Flip a byte in the first frame's CRC field ([4:8]) -> a guaranteed checksum mismatch
	// (errBadFrame), not a length change that would read as a short/torn tail (errShortFrame).
	data[4] ^= 0xFF
	if err := os.WriteFile(segs[0], data, 0o644); err != nil {
		t.Fatal(err)
	}

	if _, _, _, err := openSpool(dir, 1_000_000, 1<<30); err == nil {
		t.Fatal("openSpool must fail on checksum corruption in the active segment, not silently truncate it")
	}
}

// A torn tail on the LAST (active) segment IS an expected crash artifact: openSpool truncates it
// and recovers the good rounds (CODE_REVIEW #2 — truncation is permitted only on the active
// segment, and moved out of readSegment into openSpool).
func TestSpoolTornLastSegmentTruncates(t *testing.T) {
	dir := t.TempDir()
	sp, _, _, _ := openSpoolT(dir)
	for i := 0; i < 3; i++ {
		sp.append(int64(i), rr(fmt.Sprintf("t%d", i)))
	}
	if err := sp.flush(); err != nil {
		t.Fatal(err)
	}
	sp.close()

	segs, _ := filepath.Glob(filepath.Join(dir, "seg-*.log"))
	if len(segs) != 1 {
		t.Fatalf("want 1 segment, got %d", len(segs))
	}
	fiBefore, _ := os.Stat(segs[0])
	torn := encodeFrame(nil, 3, []byte("partial"))[:6] // a half-written frame, as a crash mid-append leaves
	f, err := os.OpenFile(segs[0], os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(torn); err != nil {
		t.Fatal(err)
	}
	f.Close()

	sp2, head2, live2, err := openSpool(dir, 1_000_000, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	defer sp2.close()
	if len(live2) != 3 || head2 != 0 {
		t.Fatalf("recovered %d rounds head=%d, want 3 rounds head 0", len(live2), head2)
	}
	if fiAfter, _ := os.Stat(segs[0]); fiAfter.Size() != fiBefore.Size() {
		t.Fatalf("torn tail not truncated back to the last good frame: size=%d, want %d", fiAfter.Size(), fiBefore.Size())
	}
}

// A non-contiguous sequence (a gap the per-frame CRC can't catch) breaks the buffer's adoption,
// so openSpool must reject it (CODE_REVIEW #2).
func TestSpoolNonContiguousSequenceFails(t *testing.T) {
	dir := t.TempDir()
	sp, _, _, _ := openSpoolT(dir)
	sp.append(0, rr("a"))
	sp.append(2, rr("c")) // seq 1 missing
	if err := sp.flush(); err != nil {
		t.Fatal(err)
	}
	sp.close()
	if _, _, _, err := openSpool(dir, 1_000_000, 1<<30); err == nil {
		t.Fatal("openSpool must fail on a non-contiguous sequence")
	}
}
