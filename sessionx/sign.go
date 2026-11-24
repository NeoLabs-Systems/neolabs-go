// Package sessionx implements server-side cookie sessions that are wire- and
// storage-compatible with express-session + better-sqlite3-session-store, so
// users stay signed in across the Node → Go switch, and the other NeoLabs apps
// share one hardened session implementation.
package sessionx

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"net/url"
	"strings"
)

const signedPrefix = "s:"

// Sign returns value + "." + HMAC-SHA256(value, secret) in unpadded base64,
// matching the npm cookie-signature package.
func Sign(value, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(value))
	return value + "." + strings.TrimRight(base64.StdEncoding.EncodeToString(mac.Sum(nil)), "=")
}

// Unsign verifies a signed value against any of the secrets (first one signs,
// the rest allow rotation) and returns the original value.
func Unsign(signed string, secrets ...string) (string, bool) {
	dot := strings.LastIndex(signed, ".")
	if dot < 0 {
		return "", false
	}
	value := signed[:dot]
	for _, secret := range secrets {
		if subtle.ConstantTimeCompare([]byte(Sign(value, secret)), []byte(signed)) == 1 {
			return value, true
		}
	}
	return "", false
}

// EncodeCookieValue is the cookie value express-session sends: "s:" + signed
// session id, percent-encoded like encodeURIComponent.
func EncodeCookieValue(sid, secret string) string {
	return url.QueryEscape(signedPrefix + Sign(sid, secret))
}

// DecodeCookieValue extracts the session id from a cookie value, or reports
// false for a missing prefix or a bad signature.
func DecodeCookieValue(raw string, secrets ...string) (string, bool) {
	decoded, err := url.QueryUnescape(raw)
	if err != nil || !strings.HasPrefix(decoded, signedPrefix) {
		return "", false
	}
	return Unsign(strings.TrimPrefix(decoded, signedPrefix), secrets...)
}
