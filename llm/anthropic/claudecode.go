package anthropic

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/NeoLabs-Systems/neolabs-go/llm"
	"github.com/NeoLabs-Systems/neolabs-go/llm/oauth"
	"github.com/NeoLabs-Systems/neolabs-go/llm/transport"
)

const (
	claudeCodeVersion      = "2.1.75"
	claudeCodeBeta         = "claude-code-20250219,oauth-2025-04-20,fine-grained-tool-streaming-2025-05-14"
	claudeCodeSystemPrompt = "You are Claude Code, Anthropic's official CLI for Claude."
	claudeCodeClientID     = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	claudeCodeTokenURL     = "https://platform.claude.com/v1/oauth/token"
	// ClaudeCodeScopes are the scopes `neoagent login claude-code` asks for.
	ClaudeCodeScopes = "user:inference user:profile org:create_api_key user:sessions:claude_code user:mcp_servers"
)

// ClaudeCodeConfig configures a Claude Code subscription.
type ClaudeCodeConfig struct {
	transport.Config
	// Credentials is the shared record of the login (access token, refresh
	// token). Without a refresh token the access token is used until it fails.
	Credentials *oauth.Credentials
	// Version is the claude-cli version reported in the user agent.
	Version string
	// TokenURL replaces the OAuth token endpoint (tests).
	TokenURL string
}

func mergeAnthropicBeta(existing string) string {
	seen := map[string]bool{}
	var out []string
	for _, item := range append(strings.Split(existing, ","), strings.Split(claudeCodeBeta, ",")...) {
		item = strings.TrimSpace(item)
		if item == "" || seen[item] {
			continue
		}
		seen[item] = true
		out = append(out, item)
	}
	return strings.Join(out, ",")
}

func isAuthenticationError(err error) bool {
	e := llm.AsError(err)
	return e != nil && (e.Status == 401 || strings.Contains(e.Message, `"authentication_error"`))
}

func isInferenceScopeError(err error) bool {
	e := llm.AsError(err)
	return e != nil && e.Status == 403 && strings.Contains(e.Message, `"permission_error"`) &&
		strings.Contains(e.Message, "scope requirement") && strings.Contains(e.Message, "user:inference")
}

// credentialError says how to fix a token that lacks the inference scope.
func credentialError(err error) error {
	if err != nil && isInferenceScopeError(err) {
		return fmt.Errorf("Claude Code OAuth token is missing inference scope. Re-run `neoagent login claude-code` to create a token with %s.", ClaudeCodeScopes)
	}
	return err
}

// NewClaudeCode is Claude through a Claude Code subscription: OAuth bearer
// authentication, the Claude Code beta surface and system prompt, and one token
// refresh when the API refuses the access token.
func NewClaudeCode(cfg ClaudeCodeConfig) *Provider {
	creds := cfg.Credentials
	if creds == nil {
		creds = oauth.New(oauth.Tokens{Access: cfg.APIKey}, nil)
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = defaultBaseURL
		cfg.TrustedBaseURL = true
	}
	version := cfg.Version
	if version == "" {
		version = claudeCodeVersion
	}
	p := New(cfg.Config)
	p.auth = func() map[string]string {
		if token := creds.Get().Access; token != "" {
			return map[string]string{"Authorization": "Bearer " + token}
		}
		return nil
	}
	headers := map[string]string{}
	for k, v := range cfg.Headers {
		headers[k] = v
	}
	headers["accept"] = "application/json"
	headers["anthropic-dangerous-direct-browser-access"] = "true"
	headers["anthropic-beta"] = mergeAnthropicBeta(cfg.Headers["anthropic-beta"])
	headers["user-agent"] = "claude-cli/" + version
	headers["x-app"] = "cli"
	p.headers = headers
	p.system = func(blocks []any) []any {
		return append([]any{transport.NewObj().Set("type", "text").Set("text", claudeCodeSystemPrompt)}, blocks...)
	}
	tokenURL := cfg.TokenURL
	if tokenURL == "" {
		tokenURL = claudeCodeTokenURL
	}
	hc := p.hc
	p.recoverAuth = func(ctx context.Context, err error) bool {
		if (!isAuthenticationError(err) && !isInferenceScopeError(err)) || !creds.Refreshable() {
			return false
		}
		_, refreshErr := creds.Refresh(ctx, func(ctx context.Context, refreshToken string) (oauth.Tokens, error) {
			return refreshClaudeCodeToken(ctx, hc, tokenURL, refreshToken)
		})
		return refreshErr == nil
	}
	p.wrapErr = credentialError
	return p
}

func refreshClaudeCodeToken(ctx context.Context, hc *http.Client, tokenURL, refreshToken string) (oauth.Tokens, error) {
	body, _ := transport.Marshal(transport.NewObj().Set("grant_type", "refresh_token").Set("refresh_token", refreshToken).Set("client_id", claudeCodeClientID))
	res, err := transport.FetchText(ctx, hc, transport.FetchOptions{
		Method: http.MethodPost, URL: tokenURL, Body: body,
		Header:   map[string]string{"Content-Type": "application/json", "Accept": "application/json", "anthropic-version": "2023-06-01"},
		MaxBytes: 256 * 1024, Timeout: 30 * time.Second, Service: "Claude Code OAuth refresh", Provider: "claude-code",
	})
	if err != nil {
		return oauth.Tokens{}, err
	}
	var data map[string]any
	if res.Text != "" && json.Unmarshal([]byte(res.Text), &data) != nil {
		data = nil
	}
	if !res.OK() {
		detail := ""
		if e, ok := data["error"].(map[string]any); ok {
			detail, _ = e["message"].(string)
		}
		for _, v := range []any{data["error_description"], data["error"], res.Text} {
			if detail != "" {
				break
			}
			detail, _ = v.(string)
		}
		if detail == "" {
			detail = "Unknown error"
		}
		if r := []rune(detail); len(r) > 2000 {
			detail = string(r[:2000])
		}
		return oauth.Tokens{}, &llm.Error{Provider: "claude-code", Status: res.Status, Retryable: llm.RetryableStatus(res.Status),
			Message: fmt.Sprintf("Claude Code OAuth refresh failed: HTTP %d %s", res.Status, detail)}
	}
	access, _ := data["access_token"].(string)
	if access == "" {
		return oauth.Tokens{}, fmt.Errorf("Claude Code OAuth refresh succeeded but no access_token was returned.")
	}
	refresh, _ := data["refresh_token"].(string)
	if refresh == "" {
		refresh = refreshToken
	}
	return oauth.Tokens{Access: access, Refresh: refresh, ExpiresAtMs: oauth.ExpiryFromResponse(data, time.Now())}, nil
}
