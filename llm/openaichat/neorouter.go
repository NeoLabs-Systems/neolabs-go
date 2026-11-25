package openaichat

import (
	"context"
	"encoding/json"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/NeoLabs-Systems/neolabs-go/llm"
	"github.com/NeoLabs-Systems/neolabs-go/llm/transport"
)

// NeoRouter's auto models: plain auto (the key's own profile), one per routing
// profile, and the same limited to free, subscription or paid models when the key
// may use more than one kind. "auto" alone is not a valid selection id, so plain
// auto is listed as "auto:default".
var autoID = regexp.MustCompile(`^auto(-(free|plan|paid))?(:|$)`)
var autoName = regexp.MustCompile(`^Auto \((.*)\)$`)

func selectableAutoID(id string) string {
	if strings.Contains(id, ":") {
		return id
	}
	return id + ":default"
}

// autoLabel: "Auto (Smartest, free models only)" becomes "NeoRouter Auto ·
// Smartest · free models only"; model pickers drop text in parentheses.
func autoLabel(model map[string]any) string {
	name := nameOr(model, strOf(model["id"]))
	if m := autoName.FindStringSubmatch(name); m != nil {
		return "NeoRouter Auto · " + strings.Join(strings.Split(m[1], ", "), " · ")
	}
	return "NeoRouter " + name
}

func strOf(v any) string {
	s, _ := v.(string)
	return s
}

// NewNeoRouter is a NeoRouter: the OpenAI API for every kind of model, routing
// each request across all of its providers. cfg.BaseURL is required; it may come
// with or without the /v1 the API lives under.
func NewNeoRouter(cfg transport.Config, cache *Cache) *Provider {
	if cache == nil {
		cache = NewCache()
	}
	if cfg.BaseURL != "" {
		cfg.BaseURL = strings.TrimSuffix(strings.TrimRight(cfg.BaseURL, "/"), "/v1") + "/v1"
	}
	p := newBase("neorouter", cfg, "")
	configureCustom(p)
	p.vision = true // NeoRouter sends images only to models that can see them
	p.window = func(model string) int { return cache.window(model, 128000) }

	apiBase := func() string { return strings.TrimRight(p.baseURL, "/") }
	call := func(ctx context.Context, path string) (map[string]any, error) {
		res, err := transport.FetchText(ctx, p.hc, transport.FetchOptions{
			URL: apiBase() + path, Header: map[string]string{"Authorization": "Bearer " + p.token(), "Accept": "application/json"},
			MaxBytes: 4 << 20, Service: "NeoRouter", Timeout: 30 * time.Second, Provider: p.name,
		})
		if err != nil {
			return nil, err
		}
		var data map[string]any
		_ = json.Unmarshal([]byte(res.Text), &data)
		if !res.OK() {
			msg := "HTTP " + strconv.Itoa(res.Status)
			if e, ok := data["error"].(map[string]any); ok {
				if m, _ := e["message"].(string); m != "" {
					msg = m
				}
			}
			return nil, &llm.Error{Provider: p.name, Status: res.Status, Retryable: llm.RetryableStatus(res.Status),
				RetryAfter: llm.RetryAfterFrom(res.Header, time.Now()), Message: "NeoRouter: " + msg}
		}
		return data, nil
	}
	list := func(ctx context.Context, kind string) ([]map[string]any, error) {
		data, err := call(ctx, "/models?type="+url.QueryEscape(kind))
		if err != nil {
			return nil, err
		}
		var out []map[string]any
		items, _ := data["data"].([]any)
		for _, item := range items {
			if m, ok := item.(map[string]any); ok {
				out = append(out, m)
			}
		}
		return out, nil
	}
	facts := func(m map[string]any) map[string]any {
		f, _ := m["neorouter"].(map[string]any)
		return f
	}

	p.catalog = func(ctx context.Context) ([]llm.CatalogModel, error) {
		models, err := list(ctx, "chat")
		if err != nil {
			return nil, err
		}
		yes := true
		var profiles, listed []llm.CatalogModel
		for _, m := range models {
			id := strOf(m["id"])
			if autoID.MatchString(id) {
				profiles = append(profiles, llm.CatalogModel{Model: llm.Model{ID: selectableAutoID(id), Name: autoLabel(m), Vision: true}, SupportsTools: &yes, Raw: m})
				continue
			}
			entry := llm.CatalogModel{Model: llm.Model{ID: id, Name: nameOr(m, id), Vision: true}, Raw: m}
			if cl, ok := m["context_length"].(float64); ok && cl > 0 {
				cache.setWindow(id, int(cl))
				entry.ContextWindow = int(cl)
			}
			f := facts(m)
			if tools, ok := f["tools"].(bool); ok {
				entry.SupportsTools = &tools
			}
			free, _ := f["free"].(bool)
			plan, _ := f["plan"].(bool)
			if free || plan {
				entry.PriceTier = "free"
			} else if pricing, ok := m["pricing"].(map[string]any); ok {
				if cost := perMillion(pricing["prompt"]); cost != nil {
					entry.PriceTier = llm.PriceTierForCost(cost)
				}
			}
			listed = append(listed, entry)
		}
		return append(profiles, listed...), nil
	}
	p.decisionModels = func(ctx context.Context) ([]llm.CatalogModel, error) {
		models, err := list(ctx, "moderation")
		if err != nil {
			return nil, err
		}
		var out []llm.CatalogModel
		for _, m := range models {
			if d, _ := facts(m)["decision"].(bool); !d {
				continue
			}
			out = append(out, llm.CatalogModel{Model: selectable(m), Raw: m})
		}
		return out, nil
	}
	p.live = func(ctx context.Context) ([]llm.CatalogModel, error) {
		models, err := list(ctx, "live")
		if err != nil {
			return nil, err
		}
		var out []llm.CatalogModel
		for _, m := range models {
			entry := llm.CatalogModel{Model: selectable(m), Raw: m}
			if protocols, ok := facts(m)["protocols"].([]any); ok {
				for _, pr := range protocols {
					if s, ok := pr.(string); ok {
						entry.Protocols = append(entry.Protocols, s)
					}
				}
			}
			out = append(out, entry)
		}
		return out, nil
	}
	p.decideURL = func() string { return apiBase() + "/systemone" }
	p.decideHeaders = func() map[string]string { return map[string]string{"Authorization": "Bearer " + p.token()} }
	return p
}

// selectable maps a NeoRouter entry to a model under its selectable id.
func selectable(m map[string]any) llm.Model {
	id := strOf(m["id"])
	if autoID.MatchString(id) {
		return llm.Model{ID: selectableAutoID(id), Name: autoLabel(m)}
	}
	return llm.Model{ID: id, Name: nameOr(m, id)}
}

// perMillion is Number(pricing.prompt) * 1e6 when that is a finite price.
func perMillion(v any) *float64 {
	var f float64
	switch t := v.(type) {
	case float64:
		f = t
	case string:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		if err != nil {
			return nil
		}
		f = parsed
	default:
		return nil
	}
	f *= 1e6
	return &f
}
