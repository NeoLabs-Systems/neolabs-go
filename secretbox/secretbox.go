// Package secretbox seals short secrets (API keys, tokens) at rest with
// AES-256-GCM. The wire layout is `<prefix>` + base64(iv(12) | tag(16) | ciphertext),
// identical to the NeoAgent `enc:v1:` values already stored in user databases.
package secretbox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

const (
	ivLen  = 12
	tagLen = 16
)

var (
	ErrNoKey   = errors.New("secretbox: key material is empty")
	ErrInvalid = errors.New("secretbox: sealed value is malformed")
)

// Box seals and opens values with one key.
type Box struct {
	prefix string
	aead   cipher.AEAD
}

// New derives the AES-256 key as SHA-256 of the trimmed key material.
func New(prefix, material string) (*Box, error) {
	material = strings.TrimSpace(material)
	if material == "" {
		return nil, ErrNoKey
	}
	sum := sha256.Sum256([]byte(material))
	block, err := aes.NewCipher(sum[:])
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Box{prefix: prefix, aead: aead}, nil
}

// IsSealed reports whether value carries this box's prefix.
func (b *Box) IsSealed(value string) bool {
	return strings.HasPrefix(value, b.prefix)
}

// Seal encrypts value. Empty input stays empty and already sealed input is
// returned unchanged, so callers can seal idempotently.
func (b *Box) Seal(value string) (string, error) {
	if value == "" || b.IsSealed(value) {
		return value, nil
	}
	iv := make([]byte, ivLen)
	if _, err := rand.Read(iv); err != nil {
		return "", fmt.Errorf("secretbox: read random iv: %w", err)
	}
	out := b.aead.Seal(nil, iv, []byte(value), nil) // ciphertext || tag
	cut := len(out) - tagLen
	payload := make([]byte, 0, len(out)+ivLen)
	payload = append(payload, iv...)
	payload = append(payload, out[cut:]...)
	payload = append(payload, out[:cut]...)
	return b.prefix + base64.StdEncoding.EncodeToString(payload), nil
}

// Open decrypts a sealed value. Values without the prefix are legacy
// plaintext and are returned as they are; use OpenSealed to reject them.
func (b *Box) Open(value string) (string, error) {
	if value == "" || !b.IsSealed(value) {
		return value, nil
	}
	return b.OpenSealed(value)
}

// OpenSealed decrypts a value and fails if it is not sealed or was tampered with.
func (b *Box) OpenSealed(value string) (string, error) {
	if !b.IsSealed(value) {
		return "", ErrInvalid
	}
	payload, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(value, b.prefix))
	if err != nil || len(payload) < ivLen+tagLen {
		return "", ErrInvalid
	}
	iv := payload[:ivLen]
	tag := payload[ivLen : ivLen+tagLen]
	ciphertext := payload[ivLen+tagLen:]
	joined := make([]byte, 0, len(ciphertext)+tagLen)
	joined = append(joined, ciphertext...)
	joined = append(joined, tag...)
	plain, err := b.aead.Open(nil, iv, joined, nil)
	if err != nil {
		return "", ErrInvalid
	}
	return string(plain), nil
}
