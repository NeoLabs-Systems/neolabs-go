// Package totp implements the authenticator-app second factor: RFC 6238 codes
// (SHA-1, 6 digits, 30 s), the otpauth URI for QR enrolment, and the one-time
// recovery codes. Behaviour matches the otplib setup of the Node servers, so
// enrolled authenticators and stored secrets keep working.
package totp

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"math/big"
	"strings"
	"time"
)

const (
	digits      = 6
	periodSecs  = 30
	toleranceS  = 30 // codes from one step before to one step after are accepted
	secretBytes = 20

	recoveryAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
)

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// GenerateSecret returns a new base32 secret (20 random bytes, no padding).
func GenerateSecret() (string, error) {
	buf := make([]byte, secretBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("totp: random secret: %w", err)
	}
	return b32.EncodeToString(buf), nil
}

func counterCode(key []byte, counter uint64) string {
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], counter)
	mac := hmac.New(sha1.New, key)
	mac.Write(msg[:])
	sum := mac.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	code := (uint32(sum[off]&0x7f) << 24) | uint32(sum[off+1])<<16 | uint32(sum[off+2])<<8 | uint32(sum[off+3])
	return fmt.Sprintf("%0*d", digits, code%1_000_000)
}

func decodeSecret(secret string) ([]byte, error) {
	key, err := b32.DecodeString(strings.ToUpper(strings.TrimRight(strings.TrimSpace(secret), "=")))
	if err != nil || len(key) < 16 {
		return nil, fmt.Errorf("totp: secret is not a valid base32 key of at least 16 bytes")
	}
	return key, nil
}

// Code returns the code valid at t.
func Code(secret string, t time.Time) (string, error) {
	key, err := decodeSecret(secret)
	if err != nil {
		return "", err
	}
	return counterCode(key, uint64(t.Unix()/periodSecs)), nil
}

// NormalizeCode trims, strips inner whitespace and upper-cases, the way a user
// might type or paste a code.
func NormalizeCode(value string) string {
	return strings.ToUpper(strings.Join(strings.Fields(value), ""))
}

// Verify reports whether token is the current code or one adjacent step away.
func Verify(secret, token string, now time.Time) bool {
	token = NormalizeCode(token)
	if len(token) != digits {
		return false
	}
	for _, c := range token {
		if c < '0' || c > '9' {
			return false
		}
	}
	key, err := decodeSecret(secret)
	if err != nil {
		return false
	}
	t := now.Unix()
	lo, hi := (t-toleranceS)/periodSecs, (t+toleranceS)/periodSecs
	ok := 0
	for c := lo; c <= hi; c++ {
		if c < 0 {
			continue
		}
		ok |= subtle.ConstantTimeCompare([]byte(token), []byte(counterCode(key, uint64(c))))
	}
	return ok == 1
}

// encodeURIComponent: everything but A-Z a-z 0-9 - _ . ! ~ * ' ( ) is percent-encoded.
func encodeURIComponent(s string) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			strings.IndexByte("-_.!~*'()", c) >= 0:
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// URI is the otpauth:// address an authenticator app scans.
func URI(issuer, label, secret string) string {
	return fmt.Sprintf("otpauth://totp/%s:%s?secret=%s&issuer=%s",
		encodeURIComponent(issuer), encodeURIComponent(label), secret, encodeURIComponent(issuer))
}

// NewRecoveryCode returns a code like "ABCDE-FGH23" (no look-alike characters).
func NewRecoveryCode() (string, error) {
	raw := make([]byte, 10)
	max := big.NewInt(int64(len(recoveryAlphabet)))
	for i := range raw {
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", fmt.Errorf("totp: random recovery code: %w", err)
		}
		raw[i] = recoveryAlphabet[n.Int64()]
	}
	return string(raw[:5]) + "-" + string(raw[5:]), nil
}

// NormalizeRecoveryCode is NormalizeCode without the dash.
func NormalizeRecoveryCode(value string) string {
	return strings.ReplaceAll(NormalizeCode(value), "-", "")
}
