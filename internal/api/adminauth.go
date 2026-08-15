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
