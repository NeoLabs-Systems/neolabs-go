package openaichat

import (
	"context"
	"regexp"
	"strings"
	"sync"

	"github.com/NeoLabs-Systems/neolabs-go/llm"
	"github.com/NeoLabs-Systems/neolabs-go/llm/systemone"
	"github.com/NeoLabs-Systems/neolabs-go/llm/transport"
)

const (
	openaiBaseURL     = "https://api.openai.com/v1"
	grokBaseURL       = "https://api.x.ai/v1"
	nvidiaBaseURL     = "https://integrate.api.nvidia.com/v1"
	nousBaseURL       = "https://inference-api.nousresearch.com/v1"
	openRouterBaseURL = "https://openrouter.ai/api/v1"
	copilotBaseURL    = "https://api.individual.githubcopilot.com"
)

// Cache holds facts one run learns about models for the next: context windows
// and reasoning metadata from catalogs, models that rejected reasoning_effort.
// Runs build fresh providers after discovery listed the models, so share one
// Cache per process. A provider given nil gets a private one.
type Cache struct {
	mu                sync.Mutex
	contextWindows    map[string]int
	reasoning         map[string]map[string]any
	noReasoningEffort map[string]bool
}

// NewCache returns an empty cache.
func NewCache() *Cache {
	return &Cache{contextWindows: map[string]int{}, reasoning: map[string]map[string]any{}, noReasoningEffort: map[string]bool{}}
}

func (c *Cache) window(model string, fallback int) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if w, ok := c.contextWindows[model]; ok {
		return w
	}
	return fallback
}

func (c *Cache) setWindow(model string, w int) {
	c.mu.Lock()
	c.contextWindows[model] = w
	c.mu.Unlock()
}

func newBase(name string, cfg transport.Config, defaultBase string) *Provider {
	base := cfg.BaseURL
	if base == "" {
		base = defaultBase
		cfg.TrustedBaseURL = cfg.TrustedBaseURL || defaultBase != ""
	}
	key := cfg.APIKey
	p := &Provider{
		name: name, hc: cfg.Client(), baseURL: base, headers: cfg.Headers,
		token:  func() string { return key },
		window: func(string) int { return 128000 },
	}
	p.messages = func(msgs []llm.Message, _ string) []any { return wireMessages(msgs, false) }
	return p
}

// New is OpenAI itself (or any service answering like it).
func New(cfg transport.Config) *Provider {
	p := newBase("openai", cfg, openaiBaseURL)
	configureOpenAI(p)
	return p
}

func configureOpenAI(p *Provider) {
	p.vision = true
	p.plainChat = true
	p.streamUsage = true
	p.messages = func(msgs []llm.Message, model string) []any { return wireMessages(msgs, IsReasoningModel(model)) }
	p.build = func(model string, msgs []any, tools []llm.Tool, opt llm.Options) *transport.Obj {
		params := transport.NewObj().Set("model", model).Set("messages", msgs)
		if IsReasoningModel(model) {
			// max_completion_tokens (not max_tokens) and no temperature for reasoning models.
			params.Set("max_completion_tokens", maxTokensOr(opt, 16384))
			if opt.ReasoningEffort != "" {
				params.Set("reasoning_effort", opt.ReasoningEffort)
			}
		} else {
			params.Set("temperature", temperatureOr(opt, 0.7))
			params.Set("max_tokens", maxTokensOr(opt, 16384))
		}
		addTools(params, tools, toolChoiceOr(opt, "auto"))
		return params
	}
	openAIDrop := regexp.MustCompile(`(?i)dall-e|whisper|tts|embed|moderat|realtime|audio|transcribe|search-api|-image-|babbage|davinci-002|^sora|-instruct|deep-research|computer-use|gpt-live`)
	p.catalog = func(ctx context.Context) ([]llm.CatalogModel, error) {
		return p.listIDs(ctx, func(id string) bool { return !openAIDrop.MatchString(id) }, "Failed to list OpenAI models")
	}
	// GPT-Live conversation models: the realtime models speak another protocol,
	// and the transcription and translation ones do not talk.
	p.live = func(ctx context.Context) ([]llm.CatalogModel, error) {
		return p.listIDs(ctx, func(id string) bool {
			return strings.HasPrefix(strings.ToLower(id), "gpt-live") && !regexp.MustCompile(`(?i)transcribe|translate`).MatchString(id)
		}, "Failed to list OpenAI live models")
	}
}

// customBuild is the plain Chat Completions request used by custom endpoints,
// Nous and NeoRouter.
func customBuild(model string, msgs []any, tools []llm.Tool, opt llm.Options) *transport.Obj {
	params := transport.NewObj().Set("model", model).Set("messages", msgs).Set("max_tokens", maxTokensOr(opt, 16384))
	if opt.Temperature != nil {
		params.Set("temperature", *opt.Temperature)
	}
	addTools(params, tools, toolChoiceOr(opt, "auto"))
	return params
}

func configureCustom(p *Provider) {
	p.build = customBuild
	p.requestPrefix = "Custom OpenAI-compatible request failed"
	p.streamPrefix = "Custom OpenAI-compatible stream failed"
	p.catalog = func(ctx context.Context) ([]llm.CatalogModel, error) {
		return p.listIDs(ctx, func(id string) bool { return strings.TrimSpace(id) != "" }, "Failed to list custom OpenAI-compatible models")
	}
}

// NewCompatible is a custom OpenAI-compatible endpoint. cfg.BaseURL is required.
func NewCompatible(cfg transport.Config) *Provider {
	p := newBase("openai-compatible", cfg, "")
	configureCustom(p)
	return p
}

// NewNous is Nous Portal, the inference service behind Hermes Agent. Its list
// also carries batch-only variants, which cannot chat.
func NewNous(cfg transport.Config) *Provider {
	p := newBase("nous", cfg, nousBaseURL)
	configureCustom(p)
	all := p.catalog
	p.catalog = func(ctx context.Context) ([]llm.CatalogModel, error) {
		models, err := all(ctx)
		if err != nil {
			return nil, err
		}
		kept := models[:0]
		for _, m := range models {
			if !strings.HasSuffix(m.ID, ":batch") {
				kept = append(kept, m)
			}
		}
		return kept, nil
	}
	return p
}

var rejectsEffort = regexp.MustCompile(`(?i)reasoning`)

// NewGrok is xAI Grok with an API key.
func NewGrok(cfg transport.Config, cache *Cache) *Provider {
	if cache == nil {
		cache = NewCache()
	}
	p := newBase("grok", cfg, grokBaseURL)
	configureGrok(p, cache)
	return p
}

func configureGrok(p *Provider, cache *Cache) {
	p.vision = true
	p.streamUsage = true
	p.window = func(string) int { return 131072 }
	p.build = func(model string, msgs []any, tools []llm.Tool, opt llm.Options) *transport.Obj {
		params := transport.NewObj().Set("model", model).Set("messages", msgs).Set("max_tokens", maxTokensOr(opt, 16384))
		if !strings.Contains(model, "reasoning") {
			params.Set("temperature", temperatureOr(opt, 0.9))
		}
		// Structured helper calls run at the engine's reasoning effort, as on
		// other providers. Agent turns keep Grok's own reasoning depth.
		cache.mu.Lock()
		skip := cache.noReasoningEffort[model]
		cache.mu.Unlock()
		if structured, _ := opt.Extra["structured"].(bool); structured && opt.ReasoningEffort != "" && !skip {
			params.Set("reasoning_effort", opt.ReasoningEffort)
		}
		addTools(params, tools, "auto")
		return params
	}
	// A model that rejects reasoning_effort is remembered and called without it.
	p.retryChat = func(err error, body *transport.Obj) *transport.Obj {
		e := llm.AsError(err)
		if e == nil || e.Status != 400 || !rejectsEffort.MatchString(e.Message) || !body.Has("reasoning_effort") {
			return nil
		}
		model, _ := body.Get("model")
		cache.mu.Lock()
		cache.noReasoningEffort[model.(string)] = true
		cache.mu.Unlock()
		retry := body.Clone()
		retry.Delete("reasoning_effort")
		return retry
	}
	grokDrop := regexp.MustCompile(`(?i)imagine|diffus|embed|-tts`)
	p.catalog = func(ctx context.Context) ([]llm.CatalogModel, error) {
		return p.listIDs(ctx, func(id string) bool { return !grokDrop.MatchString(id) }, "Failed to list Grok models")
	}
}

var nvidiaDrop = regexp.MustCompile(`(?i)embed|bge|e5-|rerank|guard|safety|moderat|diffus|flux|stable|imagen|vision-enc|whisper|tts|speech|paraphrase|classif`)
var nvidiaReasoning = regexp.MustCompile(`(?i)(?:^|[/_-])(?:r\d+|qwq|reasoning)(?:$|[/_.-])`)

// NewNvidia is NVIDIA NIM.
func NewNvidia(cfg transport.Config) *Provider {
	p := newBase("nvidia", cfg, nvidiaBaseURL)
	p.streamUsage = true
	p.window = func(string) int { return 131072 }
	p.requestPrefix = "NVIDIA NIM request failed"
	p.streamPrefix = "NVIDIA NIM stream failed"
	p.build = func(model string, msgs []any, tools []llm.Tool, opt llm.Options) *transport.Obj {
		params := transport.NewObj().Set("model", model).Set("messages", msgs).Set("max_tokens", maxTokensOr(opt, 8192))
		if !nvidiaReasoning.MatchString(model) {
			params.Set("temperature", temperatureOr(opt, 0.6))
		}
		addTools(params, tools, "auto")
		return params
	}
	p.catalog = func(ctx context.Context) ([]llm.CatalogModel, error) {
		return p.listIDs(ctx, func(id string) bool { return !nvidiaDrop.MatchString(id) }, "NVIDIA NIM request failed")
	}
	return p
}

// Decide asks a SystemOne decision model through the provider's systemone endpoint.
func (p *Provider) Decide(ctx context.Context, req llm.DecisionRequest) (map[string]any, error) {
	if p.decideURL == nil {
		return nil, &llm.Error{Provider: p.name, Message: "Provider '" + p.name + "' does not serve decision models"}
	}
	return systemone.Post(ctx, p.hc, p.name, p.decideURL(), p.decideHeaders(), req)
}

// DecisionModels lists the SystemOne models the provider serves.
func (p *Provider) DecisionModels(ctx context.Context) ([]llm.Model, error) {
	entries, err := p.DecisionCatalog(ctx)
	if err != nil {
		return nil, err
	}
	models := make([]llm.Model, len(entries))
	for i, e := range entries {
		models[i] = e.Model
	}
	return models, nil
}

// DecisionCatalog lists the SystemOne models with everything the vendor stated.
func (p *Provider) DecisionCatalog(ctx context.Context) ([]llm.CatalogModel, error) {
	if p.decisionModels == nil {
		return nil, nil
	}
	return p.decisionModels(ctx)
}
