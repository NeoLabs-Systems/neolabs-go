// Package passhash holds the credential primitives every NeoLabs app shares:
// bcrypt password hashes (compatible with existing `$2b$` hashes), random
// tokens, SHA-256 token digests and constant-time comparison.
package passhash

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"

	"golang.org/x/crypto/bcrypt"
)

// DefaultCost is the bcrypt work factor used for new hashes.
const DefaultCost = 12

// bcrypt only reads the first 72 bytes. The Node implementation truncated
// silently, so existing hashes were produced from the truncated input.
const maxPasswordBytes = 72

func truncate(password string) []byte {
	b := []byte(password)
	if len(b) > maxPasswordBytes {
		b = b[:maxPasswordBytes]
	}
	return b
}

// Hash returns a bcrypt hash of password.
func Hash(password string, cost int) (string, error) {
	out, err := bcrypt.GenerateFromPassword(truncate(password), cost)
	if err != nil {
		return "", fmt.Errorf("passhash: hash: %w", err)
	}
	return string(out), nil
}

// Verify reports whether password matches hash. Malformed hashes never match.
func Verify(hash, password string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), truncate(password)) == nil
}

// NeedsRehash reports whether hash was made with a lower cost than wanted.
func NeedsRehash(hash string, cost int) bool {
	have, err := bcrypt.Cost([]byte(hash))
	return err != nil || have < cost
}

// RandomToken returns n random bytes, base64url without padding.
func RandomToken(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("passhash: random token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// DigestToken returns the hex SHA-256 of a bearer token. Only the digest is
// stored, so a leaked database does not leak usable tokens.
func DigestToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// Equal compares two strings in constant time.
func Equal(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
