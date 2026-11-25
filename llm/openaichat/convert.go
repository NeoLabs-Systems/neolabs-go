package openaichat

import (
	"errors"
	"strings"

	"github.com/NeoLabs-Systems/neolabs-go/llm"
	"github.com/NeoLabs-Systems/neolabs-go/llm/transport"
)

// reasoningKeys are the reasoning fields an OpenAI-style API takes back on an
// assistant message. DeepSeek rejects a tool-call turn that comes back without
// its reasoning_content; OpenRouter wants reasoning_details exactly as sent.
var reasoningKeys = []string{"reasoning_details", "reasoning_content", "reasoning"}

// wireMessages turns the harness transcript into API messages. Reasoning state
// is merged into its message (only the fields above); provider content blocks
// of other providers never leave the process. developerRole renames the system
// role for reasoning models.
func wireMessages(msgs []llm.Message, developerRole bool) []any {
	out := make([]any, 0, len(msgs))
	for _, m := range msgs {
		role := m.Role
		if developerRole && role == "system" {
			role = "developer"
		}
		o := transport.NewObj().Set("role", role)
		if m.Content != nil {
			o.Set("content", m.Content)
		}
		if m.Name != "" {
			o.Set("name", m.Name)
		}
		if m.ToolCallID != "" {
			o.Set("tool_call_id", m.ToolCallID)
		}
		if len(m.ToolCalls) > 0 {
			o.Set("tool_calls", wireToolCalls(m.ToolCalls))
		}
		for _, key := range reasoningKeys {
			if v, ok := m.ReasoningState[key]; ok {
				o.Set(key, v)
			}
		}
		out = append(out, o)
	}
	return out
}

func wireToolCalls(calls []llm.ToolCall) []any {
	out := make([]any, len(calls))
	for i, tc := range calls {
		typ := tc.Type
		if typ == "" {
			typ = "function"
		}
		out[i] = transport.NewObj().
			Set("id", tc.ID).
			Set("type", typ).
			Set("function", transport.NewObj().Set("name", tc.Function.Name).Set("arguments", tc.Function.Arguments))
	}
	return out
}

// formatTools lists the declared tools in the function-calling shape.
func formatTools(tools []llm.Tool) []any {
	out := make([]any, 0, len(tools))
	for _, t := range tools {
		name := strings.TrimSpace(t.Name)
		if name == "" {
			continue
		}
		fn := transport.NewObj().Set("name", name)
		if t.Description != "" {
			fn.Set("description", t.Description)
		}
		fn.Set("parameters", toolParameters(t))
		out = append(out, transport.NewObj().Set("type", "function").Set("function", fn))
	}
	return out
}

func toolParameters(t llm.Tool) any {
	if t.Parameters == nil {
		return transport.NewObj().Set("type", "object").Set("properties", transport.NewObj())
	}
	return t.Parameters
}

// toolChoiceOr is options.toolChoice || fallback.
func toolChoiceOr(opt llm.Options, fallback string) any {
	if v, ok := opt.Extra["toolChoice"]; ok && v != nil && v != "" {
		return v
	}
	return fallback
}

func maxTokensOr(opt llm.Options, fallback int) int {
	if opt.MaxTokens > 0 {
		return opt.MaxTokens
	}
	return fallback
}

func temperatureOr(opt llm.Options, fallback float64) float64 {
	if opt.Temperature != nil {
		return *opt.Temperature
	}
	return fallback
}

func addTools(params *transport.Obj, tools []llm.Tool, choice any) {
	if len(tools) > 0 {
		params.Set("tools", formatTools(tools))
		params.Set("tool_choice", choice)
	}
}

func textField(v any) string {
	s, _ := v.(string)
	return s
}

// usageFrom is normalizeUsage: OpenAI-style usage objects, with the fallbacks
// other services use (input_tokens_details, camelCase).
func usageFrom(u map[string]any) *llm.Usage {
	if u == nil {
		return nil
	}
	pick := func(candidates ...any) int {
		for _, c := range candidates {
			if f, ok := c.(float64); ok {
				return int(f)
			}
		}
		return 0
	}
	nested := func(key, field string) any {
		m, _ := u[key].(map[string]any)
		return m[field]
	}
	return &llm.Usage{
		InputTokens:      pick(u["prompt_tokens"], u["promptTokens"]),
		OutputTokens:     pick(u["completion_tokens"], u["completionTokens"]),
		ReasoningTokens:  pick(nested("completion_tokens_details", "reasoning_tokens"), nested("output_tokens_details", "reasoning_tokens"), u["reasoningTokens"]),
		CachedReadTokens: pick(nested("prompt_tokens_details", "cached_tokens"), nested("input_tokens_details", "cached_tokens"), u["cachedReadTokens"]),
		CacheWriteTokens: pick(nested("prompt_tokens_details", "cache_write_tokens"), nested("input_tokens_details", "cache_write_tokens"), u["cacheWriteTokens"]),
	}
}

// reasoningState carries a provider's reasoning in the field it used, so it
// goes back the same way.
func reasoningState(reasoningContent, reasoning string, details []map[string]any) map[string]any {
	var kept []any
	for _, d := range details {
		if d != nil {
			kept = append(kept, d)
		}
	}
	switch {
	case len(kept) > 0:
		return map[string]any{"reasoning_details": kept}
	case reasoningContent != "":
		return map[string]any{"reasoning_content": reasoningContent}
	case reasoning != "":
		return map[string]any{"reasoning": reasoning}
	}
	return nil
}

func (p *Provider) normalizeResponse(resp map[string]any) (*llm.Result, error) {
	choices, _ := resp["choices"].([]any)
	var choice map[string]any
	if len(choices) > 0 {
		choice, _ = choices[0].(map[string]any)
	}
	if choice == nil {
		return nil, errors.New("Provider '" + p.name + "' returned no choices in the response")
	}
	msg, _ := choice["message"].(map[string]any)
	finish, _ := choice["finish_reason"].(string)
	usage, _ := resp["usage"].(map[string]any)
	servedModel, _ := resp["model"].(string)
	var calls []llm.ToolCall
	rawCalls, _ := msg["tool_calls"].([]any)
	for _, raw := range rawCalls {
		tc, _ := raw.(map[string]any)
		fn, _ := tc["function"].(map[string]any)
		if fn == nil {
			continue
		}
		calls = append(calls, llm.ToolCall{ID: textField(tc["id"]), Type: "function", Function: llm.FunctionCall{Name: textField(fn["name"]), Arguments: textField(fn["arguments"])}})
	}
	result := &llm.Result{
		Content:      textField(msg["content"]),
		Model:        servedModel,
		ToolCalls:    calls,
		FinishReason: finish,
		Usage:        usageFrom(usage),
	}
	if p.plainChat {
		// OpenAI chat reports no truncation and no reasoning state.
		return result, nil
	}
	result.Truncated = finish == "length"
	if len(rawCalls) > 0 {
		var details []map[string]any
		if list, ok := msg["reasoning_details"].([]any); ok {
			for _, d := range list {
				m, _ := d.(map[string]any)
				details = append(details, m)
			}
		}
		result.ReasoningState = reasoningState(textField(msg["reasoning_content"]), textField(msg["reasoning"]), details)
	}
	return result, nil
}
