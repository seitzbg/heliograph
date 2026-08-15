package api

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestParseAdminSessionKey(t *testing.T) {
	encoded := strings.Repeat("ab", adminSessionKeyBytes)
	k1, err := ParseAdminSessionKey(encoded)
	if err != nil {
		t.Fatalf("ParseAdminSessionKey: %v", err)
	}
	k2, err := ParseAdminSessionKey(encoded)
	if err != nil || !bytes.Equal(k1, k2) {
		t.Fatalf("same persisted secret did not reproduce the signing key: equal=%v err=%v", bytes.Equal(k1, k2), err)
	}
	if len(k1) != adminSessionKeyBytes {
		t.Errorf("key length = %d, want %d", len(k1), adminSessionKeyBytes)
	}
	for _, bad := range []string{"", "abcd", strings.Repeat("zz", adminSessionKeyBytes), strings.Repeat("ab", adminSessionKeyBytes+1)} {
		if _, err := ParseAdminSessionKey(bad); err == nil {
			t.Errorf("ParseAdminSessionKey(%q) succeeded, want an error", bad)
		}
	}

	// End to end: a session signed before a "restart" still verifies after loading the same
	// independent key; rotating that key invalidates it without exposing the login password.
	now := time.Unix(1_700_000_000, 0)
	tok := signSession(k1, now.Add(time.Hour))
	if !verifySession(k2, tok, now) {
		t.Error("session did not survive a simulated restart (re-loaded key)")
	}
	rotated, _ := ParseAdminSessionKey(strings.Repeat("cd", adminSessionKeyBytes))
	if verifySession(rotated, tok, now) {
		t.Error("session verified under a rotated key")
	}
}

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
