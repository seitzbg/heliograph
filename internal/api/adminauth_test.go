package api

import (
	"testing"
	"time"
)

func TestSessionSignVerify(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	now := time.Unix(1_700_000_000, 0)
	tok := signSession(key, now.Add(time.Hour))

	if !verifySession(key, tok, now) {
		t.Error("valid token rejected")
	}
	if verifySession(key, tok, now.Add(2*time.Hour)) {
		t.Error("expired token accepted")
	}
	if verifySession([]byte("different-key-different-key-xxxx"), tok, now) {
		t.Error("token accepted under a different key")
	}
	if verifySession(key, tok+"0", now) {
		t.Error("tampered signature accepted")
	}
	if verifySession(key, "not-a-token", now) {
		t.Error("garbage token accepted")
	}
}
