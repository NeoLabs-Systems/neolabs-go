// Package oauth holds the credentials of an OAuth login that provider clients
// share. Refresh tokens rotate, so every client of one login must read the same
// record and a refresh must be spent once, not once per client.
package oauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/NeoLabs-Systems/neolabs-go/llm/transport"
)

// Tokens are one OAuth grant. ExpiresAtMs is unix milliseconds, 0 if unknown.
type Tokens struct {
	Access      string
	Refresh     string
	ExpiresAtMs int64
}

// Credentials is the shared, mutable record of one login. Persist (optional) is
// called after every refresh so the application can store the new tokens.
type Credentials struct {
	mu      sync.Mutex
	tokens  Tokens
	persist func(Tokens)
	flight  transport.Flight[Tokens]
}

// New returns credentials holding t.
func New(t Tokens, persist func(Tokens)) *Credentials {
	return &Credentials{tokens: t, persist: persist}
}

// Get returns the current tokens.
func (c *Credentials) Get() Tokens {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.tokens
}

// Refreshable reports whether a refresh token is held.
func (c *Credentials) Refreshable() bool { return c.Get().Refresh != "" }

// Expiring reports whether the access token ends within skew (false if unknown).
func (c *Credentials) Expiring(now time.Time, skew time.Duration) bool {
	t := c.Get()
	return t.ExpiresAtMs > 0 && t.ExpiresAtMs <= now.Add(skew).UnixMilli()
}

// Refresh exchanges the refresh token through fn once, however many callers ask
// at the same time, stores the result and returns it. The old refresh token is
// kept when the response carries none.
func (c *Credentials) Refresh(ctx context.Context, fn func(ctx context.Context, refreshToken string) (Tokens, error)) (Tokens, error) {
	current := c.Get()
	if current.Refresh == "" {
		return current, nil
	}
	return c.flight.Do(ctx, current.Refresh, func() (Tokens, error) {
		fresh, err := fn(context.WithoutCancel(ctx), current.Refresh)
		if err != nil {
			return Tokens{}, err
		}
		if fresh.Refresh == "" {
			fresh.Refresh = current.Refresh
		}
		c.mu.Lock()
		c.tokens = fresh
		c.mu.Unlock()
		if c.persist != nil {
			c.persist(fresh)
		}
		return fresh, nil
	})
}

// JWTExpiryMs reads the exp claim of a JWT in milliseconds.
func JWTExpiryMs(token string) (int64, bool) {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return 0, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return 0, false
	}
	var payload struct {
		Exp float64 `json:"exp"`
	}
	if json.Unmarshal(raw, &payload) != nil || payload.Exp <= 0 || math.IsNaN(payload.Exp) {
		return 0, false
	}
	return int64(payload.Exp * 1000), true
}

// JWTClaims decodes the payload of a JWT, or nil.
func JWTClaims(token string) map[string]any {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return nil
	}
	var claims map[string]any
	if json.Unmarshal(raw, &claims) != nil {
		return nil
	}
	return claims
}

// ExpiryFromResponse reads expires_at / expiresAt (seconds or milliseconds) or
// expires_in (seconds from now) from a token response, in milliseconds.
func ExpiryFromResponse(data map[string]any, now time.Time) int64 {
	number := func(keys ...string) (float64, bool) {
		for _, k := range keys {
			if f, ok := data[k].(float64); ok && !math.IsNaN(f) && !math.IsInf(f, 0) {
				return f, true
			}
		}
		return 0, false
	}
	if at, ok := number("expires_at", "expiresAt"); ok && at > 0 {
		if at > 10_000_000_000 {
			return int64(at)
		}
		return int64(at * 1000)
	}
	if in, ok := number("expires_in", "expiresIn"); ok && in > 0 {
		return now.UnixMilli() + int64(in*1000)
	}
	return 0
}
