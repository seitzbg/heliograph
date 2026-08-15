package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"strconv"
	"strings"
	"time"
)

// deriveAdminKey derives the session-signing key deterministically from the admin password, so
// admin logins survive a collector restart. (A random per-process key logged every admin out on
// every restart.) Changing SMOKED_ADMIN_PASSWORD rotates the key and invalidates existing sessions
// — the right behavior. The password already grants full admin, so deriving the HMAC key from it
// adds no exposure; the domain-separation prefix keeps this key distinct from any other use.
func DeriveAdminKey(password string) []byte {
	sum := sha256.Sum256([]byte("heliograph/admin-session-key/v1\x00" + password))
	return sum[:]
}

// signSession returns an opaque bearer token "<payload>.<hmac>" carrying an expiry, signed with
// the admin session key (see deriveAdminKey). It is stored in the admin session cookie.
func signSession(key []byte, exp time.Time) string {
	payload := base64.RawURLEncoding.EncodeToString([]byte("exp=" + strconv.FormatInt(exp.Unix(), 10)))
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(payload))
	return payload + "." + hex.EncodeToString(mac.Sum(nil))
}

// verifySession reports whether token has a valid signature (constant-time) and has not
// expired as of now.
func verifySession(key []byte, token string, now time.Time) bool {
	dot := strings.IndexByte(token, '.')
	if dot < 0 {
		return false
	}
	payload, sig := token[:dot], token[dot+1:]
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(payload))
	want := hex.EncodeToString(mac.Sum(nil))
	if subtle.ConstantTimeCompare([]byte(sig), []byte(want)) != 1 {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil || !strings.HasPrefix(string(raw), "exp=") {
		return false
	}
	sec, err := strconv.ParseInt(strings.TrimPrefix(string(raw), "exp="), 10, 64)
	if err != nil {
		return false
	}
	return now.Unix() < sec
}
