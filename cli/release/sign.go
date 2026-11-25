package release

import (
	"bytes"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// spkiPrefix is the DER prefix of an Ed25519 SubjectPublicKeyInfo.
var spkiPrefix = []byte{0x30, 0x2a, 0x30, 0x05, 0x06, 0x03, 0x2b, 0x65, 0x70, 0x03, 0x21, 0x00}

// ErrKeyRequired is returned for an empty key.
var ErrKeyRequired = errors.New("a signing key is required")

// ParsePublicKey reads a base64 Ed25519 public key, either the raw 32 bytes
// or a DER SubjectPublicKeyInfo.
func ParsePublicKey(encoded string) (ed25519.PublicKey, error) {
	encoded = strings.TrimSpace(encoded)
	if encoded == "" {
		return nil, ErrKeyRequired
	}
	der, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("signing public key: %w", err)
	}
	if len(der) == ed25519.PublicKeySize {
		return ed25519.PublicKey(der), nil
	}
	if len(der) == len(spkiPrefix)+ed25519.PublicKeySize && bytes.HasPrefix(der, spkiPrefix) {
		return ed25519.PublicKey(der[len(spkiPrefix):]), nil
	}
	return nil, errors.New("signing public key must be a raw 32-byte Ed25519 key")
}

// NormalizePublicKey returns the key as base64 of its raw 32 bytes.
func NormalizePublicKey(encoded string) (string, error) {
	key, err := ParsePublicKey(encoded)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(key), nil
}

// ParsePrivateKey reads a base64 PKCS#8 DER Ed25519 private key.
func ParsePrivateKey(encoded string) (ed25519.PrivateKey, error) {
	encoded = strings.TrimSpace(encoded)
	if encoded == "" {
		return nil, ErrKeyRequired
	}
	der, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("signing private key: %w", err)
	}
	key, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return nil, fmt.Errorf("signing private key: %w", err)
	}
	priv, ok := key.(ed25519.PrivateKey)
	if !ok {
		return nil, errors.New("signing private key is not an Ed25519 key")
	}
	return priv, nil
}

// Sign returns the base64 Ed25519 signature of data.
func Sign(data []byte, privateKey string) (string, error) {
	priv, err := ParsePrivateKey(privateKey)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(ed25519.Sign(priv, data)), nil
}

// Verify checks a base64 signature of data against a base64 public key.
func Verify(data []byte, signature, publicKey string) (bool, error) {
	pub, err := ParsePublicKey(publicKey)
	if err != nil {
		return false, err
	}
	sig, err := base64.StdEncoding.DecodeString(strings.TrimSpace(signature))
	if err != nil {
		return false, nil
	}
	return ed25519.Verify(pub, data, sig), nil
}

// CheckKeypair fails unless privateKey signs what publicKey verifies.
func CheckKeypair(privateKey, publicKey string) error {
	probe := []byte("neolabs-release-signing-probe\n")
	sig, err := Sign(probe, privateKey)
	if err != nil {
		return err
	}
	ok, err := Verify(probe, sig, publicKey)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("The signing private key does not match the embedded public key.")
	}
	return nil
}
