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

// signSession returns an opaque bearer token "<payload>.<hmac>" carrying an expiry, signed
// with a random per-process key. It is stored in the admin session cookie.
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
