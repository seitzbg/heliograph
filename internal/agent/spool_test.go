package agent

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
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
