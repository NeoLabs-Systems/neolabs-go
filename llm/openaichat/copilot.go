package openaichat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/NeoLabs-Systems/neolabs-go/llm"
	"github.com/NeoLabs-Systems/neolabs-go/llm/transport"
)

const copilotTokenURL = "https://api.github.com/copilot_internal/v2/token"

// CopilotConfig configures GitHub Copilot. APIKey is the GitHub access token the
// short-lived Copilot token is exchanged for.
type CopilotConfig struct {
	transport.Config
	// TokenURL replaces the GitHub token endpoint (tests).
	TokenURL string
}

type copilotAuth struct {
	mu          sync.Mutex
	githubToken string
	token       string
	expiresAt   int64 // unix seconds
	tokenURL    string
	fetch       func(ctx context.Context) (*transport.TextResponse, error)
	flight      transport.Flight[struct{}]
}

func (a *copilotAuth) current() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.token != "" {
		return a.token
	}
	return a.githubToken
}

// refresh exchanges the GitHub token for a Copilot token when none is held or it
// ends within five minutes.
func (a *copilotAuth) refresh(ctx context.Context) error {
	a.mu.Lock()
	fresh := a.token != "" && a.expiresAt >= time.Now().Unix()+300
	a.mu.Unlock()
	if fresh {
		return nil
	}
	_, err := a.flight.Do(ctx, "refresh", func() (struct{}, error) { return struct{}{}, a.exchange(context.WithoutCancel(ctx)) })
	return err
}

func (a *copilotAuth) exchange(ctx context.Context) error {
	if a.githubToken == "" {
		return errors.New("GitHub Copilot access token is missing. Please run `neoagent login github-copilot`.")
	}
	res, err := a.fetch(ctx)
	if err != nil {
		return err
	}
	if !res.OK() {
		text := res.Text
		if len(text) > 500 {
			text = text[:500]
		}
		return &llm.Error{Provider: "github-copilot", Status: res.Status, Retryable: llm.RetryableStatus(res.Status),
			RetryAfter: llm.RetryAfterFrom(res.Header, time.Now()),
			Message:    fmt.Sprintf("Failed to refresh GitHub Copilot token: HTTP %d - %s", res.Status, llm.SanitizeDetail(text))}
	}
	var data struct {
		Token     string `json:"token"`
		ExpiresAt any    `json:"expires_at"`
	}
	body := res.Text
	if body == "" {
		body = "{}"
	}
	if json.Unmarshal([]byte(body), &data) != nil || data.Token == "" {
		return errors.New("Invalid token response from GitHub Copilot.")
	}
	expires := time.Now().Unix() + 1800
	switch v := data.ExpiresAt.(type) {
	case float64:
		expires = int64(v)
	case string:
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			expires = int64(math.Floor(float64(t.UnixMilli()) / 1000))
		}
	}
	a.mu.Lock()
	a.token, a.expiresAt = data.Token, expires
	a.mu.Unlock()
	return nil
}

// NewGitHubCopilot serves a GitHub Copilot subscription as an OpenAI provider.
func NewGitHubCopilot(cfg CopilotConfig) *Provider {
	if cfg.BaseURL == "" {
		cfg.BaseURL = copilotBaseURL
	}
	cfg.TrustedBaseURL = cfg.TrustedBaseURL || cfg.BaseURL == copilotBaseURL
	headers := map[string]string{
		"Editor-Version":         "vscode/1.90.0",
		"Editor-Plugin-Version":  "copilot-chat/0.15.0",
		"User-Agent":             "GithubCopilot/1.155.0",
		"X-Github-Api-Version":   "2023-07-07",
		"Copilot-Integration-Id": "vscode-chat",
	}
	for k, v := range cfg.Headers {
		headers[k] = v
	}
	cfg.Headers = headers
	p := newBase("github-copilot", cfg.Config, copilotBaseURL)
	configureOpenAI(p)
	tokenURL := cfg.TokenURL
	if tokenURL == "" {
		tokenURL = copilotTokenURL
	}
	auth := &copilotAuth{githubToken: cfg.APIKey, tokenURL: tokenURL}
	hc := p.hc
	auth.fetch = func(ctx context.Context) (*transport.TextResponse, error) {
		return transport.FetchText(ctx, hc, transport.FetchOptions{
			URL: auth.tokenURL, MaxBytes: 1 << 20, Service: "GitHub Copilot token refresh", Provider: "github-copilot",
			Header: map[string]string{"Authorization": "token " + auth.githubToken, "Accept": "application/json", "User-Agent": "NeoAgent/1.0.0"},
		})
	}
	p.token = auth.current
	p.prepare = auth.refresh
	return p
}
