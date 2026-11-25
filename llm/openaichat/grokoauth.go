package openaichat

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/NeoLabs-Systems/neolabs-go/llm"
	"github.com/NeoLabs-Systems/neolabs-go/llm/oauth"
	"github.com/NeoLabs-Systems/neolabs-go/llm/transport"
)

const (
	GrokOAuthClientID   = "b1a00492-073a-47ea-816f-4c329264a828"
	grokOAuthTokenURL   = "https://auth.x.ai/oauth2/token"
	grokInvalidCredCode = "unauthenticated:bad-credentials"
	grokRefreshSkew     = 5 * time.Minute
	grokRefreshTimeout  = 30 * time.Second
	grokRefreshMaxBytes = 256 * 1024
	GrokOAuthScopes     = "openid profile email offline_access grok-cli:access api:access"
)

// GrokOAuthConfig configures Grok through an xAI account login.
type GrokOAuthConfig struct {
	transport.Config
	// Credentials is the shared record of the login; without a refresh token the
	// access token is used until it fails.
	Credentials *oauth.Credentials
	Cache       *Cache
	// TokenURL replaces the xAI token endpoint (tests).
	TokenURL string
}

// grokExpiry is the access token's own exp claim, else what the response says.
func grokExpiry(access string, response map[string]any, now time.Time) int64 {
	if exp, ok := oauth.JWTExpiryMs(access); ok {
		return exp
	}
	return oauth.ExpiryFromResponse(response, now)
}

func isGrokAuthenticationError(err error) bool {
	e := llm.AsError(err)
	return e != nil && (e.Status == 401 || (e.Status == 403 && e.Code == grokInvalidCredCode))
}

// NewGrokOAuth is Grok through an xAI account. The access token is renewed
// shortly before it expires and once more if the API refuses it.
func NewGrokOAuth(cfg GrokOAuthConfig) *Provider {
	creds := cfg.Credentials
	if creds == nil {
		creds = oauth.New(oauth.Tokens{Access: cfg.APIKey}, nil)
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = grokBaseURL
	}
	cfg.TrustedBaseURL = cfg.TrustedBaseURL || cfg.BaseURL == grokBaseURL
	cache := cfg.Cache
	if cache == nil {
		cache = NewCache()
	}
	p := newBase("grok-oauth", cfg.Config, grokBaseURL)
	configureGrok(p, cache)
	p.token = func() string { return creds.Get().Access }
	tokenURL := cfg.TokenURL
	if tokenURL == "" {
		tokenURL = grokOAuthTokenURL
	}
	hc := p.hc
	refresh := func(ctx context.Context) error {
		_, err := creds.Refresh(ctx, func(ctx context.Context, refreshToken string) (oauth.Tokens, error) {
			return refreshGrokToken(ctx, hc, tokenURL, refreshToken)
		})
		return err
	}
	p.prepare = func(ctx context.Context) error {
		if creds.Refreshable() && creds.Expiring(time.Now(), grokRefreshSkew) {
			return refresh(ctx)
		}
		return nil
	}
	p.recoverAuth = func(ctx context.Context, err error, attempted string) bool {
		if !isGrokAuthenticationError(err) || !creds.Refreshable() {
			return false
		}
		if creds.Get().Access != attempted {
			return true // another caller already renewed it
		}
		return refresh(ctx) == nil
	}
	return p
}

func refreshGrokToken(ctx context.Context, hc *http.Client, tokenURL, refreshToken string) (oauth.Tokens, error) {
	form := "grant_type=refresh_token&refresh_token=" + url.QueryEscape(refreshToken) + "&client_id=" + url.QueryEscape(GrokOAuthClientID)
	res, err := transport.FetchText(ctx, hc, transport.FetchOptions{
		Method: http.MethodPost, URL: tokenURL, Body: []byte(form),
		Header:   map[string]string{"Content-Type": "application/x-www-form-urlencoded", "Accept": "application/json"},
		MaxBytes: grokRefreshMaxBytes, Timeout: grokRefreshTimeout, Service: "Grok OAuth refresh", Provider: "grok-oauth",
	})
	if err != nil {
		return oauth.Tokens{}, err
	}
	var data map[string]any
	if res.Text != "" {
		if json.Unmarshal([]byte(res.Text), &data) != nil {
			data = nil
		}
	}
	if !res.OK() {
		if data["error"] == "invalid_grant" {
			return oauth.Tokens{}, fmt.Errorf("Grok OAuth refresh token is invalid or expired. Re-run `neoagent login grok-oauth` to re-authenticate.")
		}
		detail := ""
		for _, v := range []any{data["error_description"], data["error"], res.Text} {
			if s, _ := v.(string); s != "" {
				detail = s
				break
			}
		}
		if detail == "" {
			detail = "Unknown error"
		}
		return oauth.Tokens{}, &llm.Error{Provider: "grok-oauth", Status: res.Status, Retryable: llm.RetryableStatus(res.Status),
			Message: fmt.Sprintf("Grok OAuth refresh failed: HTTP %d %s", res.Status, truncateUnits(detail, 2000))}
	}
	access, _ := data["access_token"].(string)
	if access == "" {
		return oauth.Tokens{}, fmt.Errorf("Grok OAuth refresh succeeded but no access_token was returned.")
	}
	refresh, _ := data["refresh_token"].(string)
	if refresh == "" {
		refresh = refreshToken
	}
	return oauth.Tokens{Access: access, Refresh: refresh, ExpiresAtMs: grokExpiry(access, data, time.Now())}, nil
}

func truncateUnits(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}
