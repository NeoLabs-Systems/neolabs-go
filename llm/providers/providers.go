// Package providers builds a language-model provider by its catalog id
// ("openai", "anthropic", "ollama", ...), the Go counterpart of
// createProviderInstance. It knows how each provider is wired but nothing about
// where a deployment keeps its settings: the caller resolves API key, base URL
// and credentials (user key before server key, operator-trusted addresses) and
// passes the result in.
package providers

import (
	"fmt"

	"github.com/NeoLabs-Systems/neolabs-go/llm"
	"github.com/NeoLabs-Systems/neolabs-go/llm/anthropic"
	"github.com/NeoLabs-Systems/neolabs-go/llm/codex"
	"github.com/NeoLabs-Systems/neolabs-go/llm/google"
	"github.com/NeoLabs-Systems/neolabs-go/llm/oauth"
	"github.com/NeoLabs-Systems/neolabs-go/llm/ollama"
	"github.com/NeoLabs-Systems/neolabs-go/llm/openaichat"
	"github.com/NeoLabs-Systems/neolabs-go/llm/systemone"
	"github.com/NeoLabs-Systems/neolabs-go/llm/transport"
)

// Caches are the per-process facts providers share between runs (context
// windows, reasoning metadata, models that think). Create one and pass it to
// every New.
type Caches struct {
	Chat   *openaichat.Cache
	Google *google.Cache
}

// NewCaches returns empty caches.
func NewCaches() *Caches {
	return &Caches{Chat: openaichat.NewCache(), Google: google.NewCache()}
}

// Config is everything one provider instance needs.
type Config struct {
	transport.Config
	// Credentials is the shared OAuth record for grok-oauth, claude-code and
	// openai-codex; without it APIKey is used as a static access token.
	Credentials *oauth.Credentials
	Caches      *Caches
	// OnStatus receives Ollama model download progress.
	OnStatus func(ollama.Status)
	// ClientVersion is the NeoAgent version, reported to the Codex backend.
	ClientVersion string
}

// IDs lists the provider ids New accepts.
func IDs() []string {
	var ids []string
	for _, d := range llm.ProviderDefinitions {
		ids = append(ids, d.ID)
	}
	return ids
}

// New builds the provider with the given catalog id.
func New(id string, cfg Config) (llm.Provider, error) {
	caches := cfg.Caches
	if caches == nil {
		caches = NewCaches()
	}
	switch id {
	case "openai":
		return openaichat.New(cfg.Config), nil
	case "openai-compatible":
		return openaichat.NewCompatible(cfg.Config), nil
	case "anthropic", "minimax":
		return anthropic.New(cfg.Config), nil
	case "google":
		return google.New(cfg.Config, caches.Google), nil
	case "grok":
		return openaichat.NewGrok(cfg.Config, caches.Chat), nil
	case "grok-oauth":
		return openaichat.NewGrokOAuth(openaichat.GrokOAuthConfig{Config: cfg.Config, Credentials: cfg.Credentials, Cache: caches.Chat}), nil
	case "nvidia":
		return openaichat.NewNvidia(cfg.Config), nil
	case "nous":
		return openaichat.NewNous(cfg.Config), nil
	case "openrouter":
		return openaichat.NewOpenRouter(cfg.Config, caches.Chat), nil
	case "neorouter":
		return openaichat.NewNeoRouter(cfg.Config, caches.Chat), nil
	case "github-copilot":
		return openaichat.NewGitHubCopilot(openaichat.CopilotConfig{Config: cfg.Config}), nil
	case "openai-codex":
		return codex.New(codex.Config{Config: cfg.Config, Credentials: cfg.Credentials, Version: cfg.ClientVersion}), nil
	case "claude-code":
		return anthropic.NewClaudeCode(anthropic.ClaudeCodeConfig{Config: cfg.Config, Credentials: cfg.Credentials}), nil
	case "typesafe":
		return systemone.NewTypeSafe(cfg.Config), nil
	case "ollama":
		return ollama.New(ollama.Config{Config: cfg.Config, OnStatus: cfg.OnStatus}), nil
	}
	return nil, fmt.Errorf("Unknown provider: %s", id)
}
