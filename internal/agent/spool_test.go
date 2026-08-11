package agent

import (
	"bytes"
	"errors"
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
