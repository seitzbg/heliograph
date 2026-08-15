package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const adminSessionKeyBytes = 32

// DefaultAdminSessionTTL is how long an admin login stays valid when SMOKED_ADMIN_SESSION_TTL is
// unset — both the signed token's expiry and the cookie's Max-Age.
const DefaultAdminSessionTTL = 12 * time.Hour

// minAdminSessionTTL guards against a fat-fingered tiny value (e.g. "30" read as 30ns, or "1s")
// that would log an operator out almost immediately. There is no upper bound: a long-lived
// session is the operator's own security trade-off, and the HMAC token carries its own expiry.
const minAdminSessionTTL = time.Minute

// ParseAdminSessionTTL parses the admin session lifetime supplied through SMOKED_ADMIN_SESSION_TTL.
// It accepts any Go duration string (e.g. "12h", "24h", "30m", "168h") and requires at least one
// minute, so a Compose/K8s deployment can lengthen or shorten the login lifetime via `environment:`.
func ParseAdminSessionTTL(s string) (time.Duration, error) {
	d, err := time.ParseDuration(strings.TrimSpace(s))
	if err != nil {
		return 0, fmt.Errorf("must be a Go duration like 12h, 24h, or 30m: %w", err)
	}
	if d < minAdminSessionTTL {
		return 0, fmt.Errorf("must be at least %s, got %s", minAdminSessionTTL, d)
	}
	return d, nil
}

// ParseAdminSessionKey decodes the independent, persistent session-signing secret supplied through
// SMOKED_ADMIN_SESSION_KEY. Requiring exactly 32 random bytes (64 hex characters) keeps a weak human
// password out of the HMAC key: otherwise any captured <payload>.<signature> cookie would be a fast
// offline oracle for guesses at SMOKED_ADMIN_PASSWORD.
func ParseAdminSessionKey(encoded string) ([]byte, error) {
	key, err := hex.DecodeString(strings.TrimSpace(encoded))
	if err != nil || len(key) != adminSessionKeyBytes {
		return nil, fmt.Errorf("must be exactly %d random bytes encoded as %d hex characters", adminSessionKeyBytes, adminSessionKeyBytes*2)
	}
	return key, nil
}

// signSession returns an opaque bearer token "<payload>.<hmac>" carrying an expiry, signed with
// an independent high-entropy admin session key. It is stored in the admin session cookie.
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
