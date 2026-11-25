package llm

// ProviderDefinition describes a provider the way the settings and catalog
// screens know it. It is generated from provider_definitions.js and tested
// against it.
type ProviderDefinition struct {
	ID              string
	Label           string
	Description     string
	EnvKey          string
	BaseURLEnvKey   string
	Authentication  string // api_key, oauth, local
	SupportsAPIKey  bool
	SupportsBaseURL bool
	RequiresBaseURL bool
	// Aggregator providers resell other vendors' models, so failures are scoped
	// to the model rather than the provider.
	Aggregator     bool
	DefaultEnabled bool
	DefaultBaseURL string
}

// ProviderDefinitions lists every provider in catalog order.
var ProviderDefinitions = []ProviderDefinition{
	{ID: "openai", Label: "OpenAI", Description: "Current GPT models for general work, coding, and reasoning.", EnvKey: "OPENAI_API_KEY", BaseURLEnvKey: "OPENAI_BASE_URL", Authentication: "api_key", SupportsAPIKey: true, SupportsBaseURL: true, RequiresBaseURL: false, Aggregator: false, DefaultEnabled: true, DefaultBaseURL: ""},
	{ID: "openai-compatible", Label: "Custom OpenAI-compatible", Description: "Models exposed by a custom OpenAI-compatible Chat Completions endpoint.", EnvKey: "OPENAI_COMPATIBLE_API_KEY", BaseURLEnvKey: "OPENAI_COMPATIBLE_BASE_URL", Authentication: "api_key", SupportsAPIKey: true, SupportsBaseURL: true, RequiresBaseURL: true, Aggregator: false, DefaultEnabled: false, DefaultBaseURL: ""},
	{ID: "anthropic", Label: "Anthropic", Description: "Claude models for long-context drafting and analytical work.", EnvKey: "ANTHROPIC_API_KEY", BaseURLEnvKey: "", Authentication: "api_key", SupportsAPIKey: true, SupportsBaseURL: true, RequiresBaseURL: false, Aggregator: false, DefaultEnabled: false, DefaultBaseURL: ""},
	{ID: "google", Label: "Google", Description: "Gemini models with large context windows and multimodal support.", EnvKey: "GOOGLE_AI_KEY", BaseURLEnvKey: "", Authentication: "api_key", SupportsAPIKey: true, SupportsBaseURL: false, RequiresBaseURL: false, Aggregator: false, DefaultEnabled: true, DefaultBaseURL: ""},
	{ID: "grok", Label: "xAI", Description: "Grok models tuned for personality-heavy chat and reasoning.", EnvKey: "XAI_API_KEY", BaseURLEnvKey: "", Authentication: "api_key", SupportsAPIKey: true, SupportsBaseURL: true, RequiresBaseURL: false, Aggregator: false, DefaultEnabled: true, DefaultBaseURL: "https://api.x.ai/v1"},
	{ID: "grok-oauth", Label: "Grok (xAI OAuth)", Description: "Grok models via xAI account. Login with `neoagent login grok-oauth`.", EnvKey: "GROK_OAUTH_ACCESS_TOKEN", BaseURLEnvKey: "", Authentication: "oauth", SupportsAPIKey: true, SupportsBaseURL: false, RequiresBaseURL: false, Aggregator: false, DefaultEnabled: false, DefaultBaseURL: ""},
	{ID: "nvidia", Label: "NVIDIA NIM", Description: "NVIDIA-hosted models including free-tier Nemotron, Kimi, Llama 4, and DeepSeek. Get a key at build.nvidia.com.", EnvKey: "NVIDIA_API_KEY", BaseURLEnvKey: "", Authentication: "api_key", SupportsAPIKey: true, SupportsBaseURL: true, RequiresBaseURL: false, Aggregator: false, DefaultEnabled: false, DefaultBaseURL: "https://integrate.api.nvidia.com/v1"},
	{ID: "minimax", Label: "MiniMax Code", Description: "MiniMax Coding Plan for MiniMax-M2.7 through the Anthropic-compatible endpoint.", EnvKey: "MINIMAX_API_KEY", BaseURLEnvKey: "", Authentication: "api_key", SupportsAPIKey: true, SupportsBaseURL: true, RequiresBaseURL: false, Aggregator: false, DefaultEnabled: false, DefaultBaseURL: "https://api.minimax.io/anthropic"},
	{ID: "github-copilot", Label: "GitHub Copilot", Description: "Use your GitHub Copilot subscription as an AI provider.", EnvKey: "GITHUB_COPILOT_ACCESS_TOKEN", BaseURLEnvKey: "", Authentication: "oauth", SupportsAPIKey: true, SupportsBaseURL: true, RequiresBaseURL: false, Aggregator: false, DefaultEnabled: false, DefaultBaseURL: "https://api.githubcopilot.com"},
	{ID: "openai-codex", Label: "OpenAI Codex", Description: "Use Codex models through ChatGPT Codex authentication.", EnvKey: "OPENAI_CODEX_ACCESS_TOKEN", BaseURLEnvKey: "", Authentication: "oauth", SupportsAPIKey: true, SupportsBaseURL: true, RequiresBaseURL: false, Aggregator: false, DefaultEnabled: false, DefaultBaseURL: "https://chatgpt.com/backend-api/codex"},
	{ID: "claude-code", Label: "Claude Code", Description: "Claude models via Claude Code subscription. Login with `neoagent login claude-code`.", EnvKey: "CLAUDE_CODE_OAUTH_TOKEN", BaseURLEnvKey: "", Authentication: "oauth", SupportsAPIKey: true, SupportsBaseURL: false, RequiresBaseURL: false, Aggregator: false, DefaultEnabled: false, DefaultBaseURL: ""},
	{ID: "nous", Label: "Nous Portal", Description: "Hermes, DeepSeek, Kimi and hundreds more through Nous Research, the provider behind Hermes Agent. Get a key at portal.nousresearch.com.", EnvKey: "NOUS_API_KEY", BaseURLEnvKey: "", Authentication: "api_key", SupportsAPIKey: true, SupportsBaseURL: true, RequiresBaseURL: false, Aggregator: true, DefaultEnabled: false, DefaultBaseURL: "https://inference-api.nousresearch.com/v1"},
	{ID: "openrouter", Label: "OpenRouter", Description: "Access 300+ models through one API, including free-tier models. Get a key at openrouter.ai.", EnvKey: "OPENROUTER_API_KEY", BaseURLEnvKey: "", Authentication: "api_key", SupportsAPIKey: true, SupportsBaseURL: true, RequiresBaseURL: false, Aggregator: true, DefaultEnabled: false, DefaultBaseURL: "https://openrouter.ai/api/v1"},
	{ID: "typesafe", Label: "TypeSafe", Description: "SystemOne decision models (Jev) straight from TypeSafe. Get a key at console.typesafe.ai.", EnvKey: "TYPESAFE_API_KEY", BaseURLEnvKey: "", Authentication: "api_key", SupportsAPIKey: true, SupportsBaseURL: false, RequiresBaseURL: false, Aggregator: false, DefaultEnabled: false, DefaultBaseURL: ""},
	{ID: "neorouter", Label: "NeoRouter", Description: "Your NeoRouter: the models, voices and tools of all its providers through one connection.", EnvKey: "NEOROUTER_API_KEY", BaseURLEnvKey: "NEOROUTER_BASE_URL", Authentication: "oauth", SupportsAPIKey: true, SupportsBaseURL: true, RequiresBaseURL: true, Aggregator: false, DefaultEnabled: true, DefaultBaseURL: ""},
	{ID: "ollama", Label: "Ollama", Description: "Local models running on your machine through an Ollama server.", EnvKey: "", BaseURLEnvKey: "", Authentication: "local", SupportsAPIKey: false, SupportsBaseURL: true, RequiresBaseURL: false, Aggregator: false, DefaultEnabled: true, DefaultBaseURL: "http://localhost:11434"},
}

// ProviderDefinitionFor returns the definition of a provider id.
func ProviderDefinitionFor(id string) (ProviderDefinition, bool) {
	for _, d := range ProviderDefinitions {
		if d.ID == id {
			return d, true
		}
	}
	return ProviderDefinition{}, false
}
