package openaichat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/NeoLabs-Systems/neolabs-go/llm"
	"github.com/NeoLabs-Systems/neolabs-go/llm/transport"
)

const decisionModality = "decisions"

var attributionHeaders = map[string]string{
	"HTTP-Referer": "https://github.com/NeoLabs-Systems/NeoAgent",
	"X-Title":      "NeoAgent",
}

// catalogReasoningEffort sends effort only to models that already reason by
// default, and only at a level the catalog lists. Where reasoning is optional
// (Claude via OpenRouter, for example) the parameter would switch thinking on,
// changing latency, cost and sampling constraints instead of lowering effort.
func catalogReasoningEffort(c *Cache, model, requested string) string {
	effort := strings.ToLower(strings.TrimSpace(requested))
	c.mu.Lock()
	entry := c.reasoning[model]
	c.mu.Unlock()
	if effort == "" || entry == nil {
		return ""
	}
	if mandatory, _ := entry["mandatory"].(bool); !mandatory {
		if enabled, _ := entry["default_enabled"].(bool); !enabled {
			return ""
		}
	}
	if supported, ok := entry["supported_efforts"].([]any); ok {
		for _, s := range supported {
			if s == effort {
				return effort
			}
		}
		return ""
	}
	return effort
}

// NewOpenRouter is OpenRouter: hundreds of models through one API.
func NewOpenRouter(cfg transport.Config, cache *Cache) *Provider {
	if cache == nil {
		cache = NewCache()
	}
	p := newBase("openrouter", cfg, openRouterBaseURL)
	p.headers = map[string]string{}
	for k, v := range attributionHeaders {
		p.headers[k] = v
	}
	for k, v := range cfg.Headers {
		p.headers[k] = v
	}
	// Free models can queue; the engine retries, nothing should hang forever.
	p.headerTimeout = 90 * time.Second
	p.streamUsage = true
	p.window = func(model string) int { return cache.window(model, 128000) }
	p.requestPrefix = "OpenRouter request failed"
	p.streamPrefix = "OpenRouter stream failed"
	p.build = func(model string, msgs []any, tools []llm.Tool, opt llm.Options) *transport.Obj {
		params := transport.NewObj().
			Set("model", model).Set("messages", msgs).
			Set("temperature", temperatureOr(opt, 0.7)).
			Set("max_tokens", maxTokensOr(opt, 16384))
		if effort := catalogReasoningEffort(cache, model, opt.ReasoningEffort); effort != "" {
			params.Set("reasoning", transport.NewObj().Set("effort", effort))
		}
		// Routers such as typesafe/jev-router keep a working model for a session
		// and only switch when the gain outweighs the lost prompt cache.
		if id, _ := opt.Extra["sessionId"].(string); id != "" {
			params.Set("session_id", id)
		}
		// OpenRouter caches most providers on its own; Anthropic has to be asked.
		if strings.HasPrefix(model, "anthropic/") || strings.HasPrefix(model, "~anthropic/") {
			params.Set("cache_control", transport.NewObj().Set("type", "ephemeral"))
		}
		addTools(params, tools, "auto")
		return params
	}
	// OpenRouter answers HTTP 200 even for errors (rate limits, model unavailable).
	p.checkResponse = func(resp map[string]any) error {
		if e, ok := resp["error"]; ok && truthy(e) {
			msg := ""
			code := ""
			switch v := e.(type) {
			case string:
				msg = v
			case map[string]any:
				msg, _ = v["message"].(string)
				if msg == "" {
					raw, _ := transport.Marshal(v)
					msg = string(raw)
				}
				if c := v["code"]; truthy(c) {
					code = fmt.Sprintf(" (code %s)", jsScalar(c))
				}
			default:
				raw, _ := transport.Marshal(v)
				msg = string(raw)
			}
			return errors.New("OpenRouter: " + msg + code)
		}
		if choices, _ := resp["choices"].([]any); len(choices) == 0 {
			return errors.New("OpenRouter: model returned no choices (may be rate-limited or unavailable)")
		}
		return nil
	}
	fetch := func(ctx context.Context, query string) ([]map[string]any, error) {
		res, err := transport.FetchText(ctx, p.hc, transport.FetchOptions{
			URL: p.baseURL + "/models" + query, Header: map[string]string{"Authorization": "Bearer " + p.token()},
			MaxBytes: 5 << 20, Service: "OpenRouter model catalog", Provider: p.name,
		})
		if err != nil {
			return nil, err
		}
		if !res.OK() {
			return nil, &llm.Error{Provider: p.name, Status: res.Status, Retryable: llm.RetryableStatus(res.Status),
				RetryAfter: llm.RetryAfterFrom(res.Header, time.Now()), Message: fmt.Sprintf("OpenRouter /models returned HTTP %d", res.Status)}
		}
		text := res.Text
		if text == "" {
			text = "{}"
		}
		var payload struct {
			Data []map[string]any `json:"data"`
		}
		if json.Unmarshal([]byte(text), &payload) != nil {
			return nil, errors.New("OpenRouter /models returned invalid JSON.")
		}
		return payload.Data, nil
	}
	// Chat models only: SystemOne models cannot hold a conversation.
	p.catalog = func(ctx context.Context) ([]llm.CatalogModel, error) {
		entries, err := fetch(ctx, "")
		if err != nil {
			return nil, err
		}
		var out []llm.CatalogModel
		for _, m := range entries {
			if hasModality(m, decisionModality) {
				continue
			}
			id, _ := m["id"].(string)
			entry := llm.CatalogModel{Model: llm.Model{ID: id, Name: nameOr(m, id)}, Raw: m}
			if ctxLen, ok := m["context_length"].(float64); ok && ctxLen > 0 {
				cache.setWindow(id, int(ctxLen))
				entry.ContextWindow = int(ctxLen)
			}
			if reasoning, ok := m["reasoning"].(map[string]any); ok {
				cache.mu.Lock()
				cache.reasoning[id] = reasoning
				cache.mu.Unlock()
				entry.Reasoning = reasoning
			}
			if created, ok := m["created"].(float64); ok {
				entry.Created = int64(created)
			}
			out = append(out, entry)
		}
		return out, nil
	}
	p.decisionModels = func(ctx context.Context) ([]llm.CatalogModel, error) {
		entries, err := fetch(ctx, "?output_modalities="+decisionModality)
		if err != nil {
			return nil, err
		}
		out := make([]llm.CatalogModel, 0, len(entries))
		for _, m := range entries {
			id, _ := m["id"].(string)
			entry := llm.CatalogModel{Model: llm.Model{ID: id, Name: nameOr(m, id)}, Raw: m}
			if created, ok := m["created"].(float64); ok {
				entry.Created = int64(created)
			}
			out = append(out, entry)
		}
		return out, nil
	}
	p.decideURL = func() string { return p.baseURL + "/systemone" }
	p.decideHeaders = func() map[string]string {
		h := map[string]string{"Authorization": "Bearer " + p.token()}
		for k, v := range attributionHeaders {
			h[k] = v
		}
		return h
	}
	return p
}

func hasModality(model map[string]any, modality string) bool {
	arch, _ := model["architecture"].(map[string]any)
	list, _ := arch["output_modalities"].([]any)
	for _, m := range list {
		if m == modality {
			return true
		}
	}
	return false
}

func nameOr(m map[string]any, fallback string) string {
	if name, _ := m["name"].(string); name != "" {
		return name
	}
	return fallback
}

// jsScalar prints a JSON scalar the way a JavaScript template literal does.
func jsScalar(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		b, _ := transport.Marshal(t)
		return string(b)
	}
	b, _ := transport.Marshal(v)
	return string(b)
}
