package api

import (
	"bytes"
	"testing"
	"time"
)

func TestDeriveAdminKey(t *testing.T) {
	// Deterministic: the same password yields the same key across "restarts" — which is what
	// lets an admin session survive a collector restart (a random key would invalidate it).
	k1 := DeriveAdminKey("hunter2")
	k2 := DeriveAdminKey("hunter2")
	if !bytes.Equal(k1, k2) {
		t.Fatal("DeriveAdminKey is not deterministic for the same password")
	}
	if len(k1) != 32 {
		t.Errorf("key length = %d, want 32", len(k1))
	}
	// A different password rotates the key (so changing the password logs everyone out).
	if bytes.Equal(k1, DeriveAdminKey("hunter3")) {
		t.Error("different passwords produced the same key")
	}
	// End to end: a session signed before a "restart" still verifies after, using the re-derived
	// key; and never verifies under a key derived from a different password.
	now := time.Unix(1_700_000_000, 0)
	tok := signSession(DeriveAdminKey("hunter2"), now.Add(time.Hour))
	if !verifySession(DeriveAdminKey("hunter2"), tok, now) {
		t.Error("session did not survive a simulated restart (re-derived key)")
	}
	if verifySession(DeriveAdminKey("hunter3"), tok, now) {
		t.Error("session verified under a key derived from a different password")
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
