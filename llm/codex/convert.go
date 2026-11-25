package codex

import (
	"strings"

	"github.com/NeoLabs-Systems/neolabs-go/llm"
	"github.com/NeoLabs-Systems/neolabs-go/llm/transport"
)

const emptyInputText = " "

// normalizeContent flattens message content to plain text.
func normalizeContent(content any) string {
	switch c := content.(type) {
	case nil:
		return ""
	case string:
		return c
	case []any:
		var sb strings.Builder
		for _, part := range c {
			switch p := part.(type) {
			case string:
				sb.WriteString(p)
			case map[string]any:
				typ, _ := p["type"].(string)
				if text, ok := p["text"].(string); ok && (typ == "text" || typ == "input_text") {
					sb.WriteString(text)
				}
			}
		}
		return sb.String()
	}
	return jsString(content)
}

func jsString(v any) string {
	b, _ := transport.Marshal(v)
	return string(b)
}

func textPart(textType, text string, assistant bool, annotations any) *transport.Obj {
	o := transport.NewObj().Set("type", textType).Set("text", text)
	if assistant {
		if annotations == nil {
			annotations = []any{}
		}
		o.Set("annotations", annotations)
	}
	return o
}

func orDefault(v any, fallback any) any {
	if v == nil || v == "" || v == false {
		return fallback
	}
	return v
}

// normalizeMessageContent turns message content into Responses API parts.
func normalizeMessageContent(content any, role string) []any {
	if content == nil {
		return []any{}
	}
	assistant := role == "assistant"
	textType := "input_text"
	if assistant {
		textType = "output_text"
	}
	switch c := content.(type) {
	case string:
		return []any{textPart(textType, c, assistant, nil)}
	case []any:
		parts := []any{}
		for _, raw := range c {
			if raw == nil {
				continue
			}
			if s, ok := raw.(string); ok {
				if strings.TrimSpace(s) != "" {
					parts = append(parts, textPart(textType, s, assistant, nil))
				}
				continue
			}
			part, _ := raw.(map[string]any)
			typ, _ := part["type"].(string)
			text, hasText := part["text"].(string)
			switch {
			case (typ == "text" || typ == "input_text") && hasText:
				parts = append(parts, textPart(textType, text, assistant, nil))
			case typ == "output_text" && hasText:
				parts = append(parts, textPart(textType, text, assistant, part["annotations"]))
			case assistant && typ == "refusal":
				refusal, ok := part["refusal"].(string)
				if !ok {
					refusal, ok = part["text"].(string)
				}
				if ok {
					parts = append(parts, transport.NewObj().Set("type", "refusal").Set("refusal", refusal))
				}
			case assistant:
			case typ == "image_url":
				url := ""
				switch u := part["image_url"].(type) {
				case string:
					url = u
				case map[string]any:
					url, _ = u["url"].(string)
				}
				if url != "" {
					parts = append(parts, transport.NewObj().Set("type", "input_image").Set("image_url", url).Set("detail", orDefault(part["detail"], "auto")))
				}
			case typ == "input_image":
				parts = append(parts, transport.NewObj().Set("type", "input_image").
					Set("image_url", orDefault(part["image_url"], nil)).
					Set("file_id", orDefault(part["file_id"], nil)).
					Set("detail", orDefault(part["detail"], "auto")))
			}
		}
		return parts
	}
	text := jsString(content)
	if s, ok := content.(string); ok {
		text = s
	}
	if text == "" {
		return []any{}
	}
	return []any{textPart(textType, text, assistant, nil)}
}

func formatTools(tools []llm.Tool) []any {
	out := []any{}
	for _, t := range tools {
		name := strings.TrimSpace(t.Name)
		if name == "" {
			continue
		}
		var params any = t.Parameters
		if t.Parameters == nil {
			params = transport.NewObj().Set("type", "object").Set("properties", transport.NewObj())
		}
		out = append(out, transport.NewObj().Set("type", "function").Set("name", name).Set("description", t.Description).Set("parameters", params).Set("strict", false))
	}
	return out
}

// reasoningItems are the encrypted reasoning items an earlier answer returned.
// With store:false they are all that lets the next request continue its thinking.
func reasoningItems(m llm.Message) []any {
	items, _ := m.ReasoningState["reasoningItems"].([]any)
	out := make([]any, len(items))
	for i, item := range items {
		if fields, ok := item.(map[string]any); ok {
			item = transport.FromMapTypeFirst(fields)
		}
		out[i] = item
	}
	return out
}

func (p *Provider) buildRequest(msgs []llm.Message, tools []llm.Tool, opt llm.Options, model string) *transport.Obj {
	var instructions []string
	input := []any{}
	for _, msg := range msgs {
		if msg.Role == "" {
			continue
		}
		if (msg.Role == "system" || msg.Role == "developer") && msg.Content != nil {
			if text := strings.TrimSpace(normalizeContent(msg.Content)); text != "" {
				instructions = append(instructions, text)
			}
			continue
		}
		if msg.Role == "tool" {
			id := strings.TrimSpace(msg.ToolCallID)
			if id == "" {
				continue
			}
			input = append(input, transport.NewObj().Set("type", "function_call_output").Set("call_id", id).Set("output", normalizeContent(msg.Content)))
			continue
		}
		role := "user"
		if msg.Role == "assistant" {
			role = "assistant"
		}
		if content := normalizeMessageContent(msg.Content, role); len(content) > 0 {
			input = append(input, transport.NewObj().Set("type", "message").Set("role", role).Set("content", content))
		}
		if msg.Role == "assistant" && len(msg.ToolCalls) > 0 {
			input = append(input, reasoningItems(msg)...)
			for _, tc := range msg.ToolCalls {
				name := strings.TrimSpace(tc.Function.Name)
				callID := strings.TrimSpace(tc.ID)
				if name == "" || callID == "" {
					continue
				}
				input = append(input, transport.NewObj().Set("type", "function_call").Set("call_id", callID).Set("name", name).Set("arguments", tc.Function.Arguments))
			}
		}
	}

	effort := opt.ReasoningEffort
	if effort == "" {
		effort = "medium"
	}
	toolChoice := any("auto")
	if v, ok := opt.Extra["toolChoice"]; ok && v != nil && v != "" {
		toolChoice = v
	}
	request := transport.NewObj()
	if p.backend {
		if len(input) == 0 && len(instructions) > 0 {
			input = append(input, transport.NewObj().Set("type", "message").Set("role", "user").
				Set("content", []any{transport.NewObj().Set("type", "input_text").Set("text", emptyInputText)}))
		}
		request.Set("input", input)
		// instructions must always be present (even empty): the backend returns 400 if omitted.
		request.Set("instructions", strings.Join(instructions, "\n\n"))
		request.Set("store", false)
		if len(tools) > 0 {
			request.Set("tools", formatTools(tools)).Set("tool_choice", toolChoice)
		} else {
			request.Set("tools", []any{}).Set("tool_choice", "auto")
		}
		request.Set("parallel_tool_calls", false)
		// Reasoning parameters are only sent when there are no tools: on the Codex
		// backend, combining reasoning mode with function-calling makes the model
		// produce only internal reasoning output (no function calls, no text).
		if p.isReasoningModel(model) && len(tools) == 0 {
			request.Set("reasoning", transport.NewObj().Set("effort", effort).Set("summary", "auto"))
			request.Set("include", []any{"reasoning.encrypted_content"})
		}
		return request
	}
	request.Set("input", input)
	if len(instructions) > 0 {
		request.Set("instructions", strings.Join(instructions, "\n\n"))
	}
	if len(tools) > 0 {
		request.Set("tools", formatTools(tools)).Set("tool_choice", toolChoice)
	}
	maxTokens := opt.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 16384
	}
	request.Set("max_output_tokens", maxTokens)
	if opt.Temperature != nil {
		request.Set("temperature", *opt.Temperature)
	}
	// The engine always passes an effort, so gating on it alone sent `reasoning`
	// to non-reasoning models, which the Responses API rejects.
	if p.isReasoningModel(model) {
		request.Set("reasoning", transport.NewObj().Set("effort", effort))
	}
	return request
}

// response helpers

func responseText(resp *transport.Obj) string {
	if s := resp.String("output_text"); s != "" {
		return s
	}
	var sb strings.Builder
	for _, item := range resp.List("output") {
		it, _ := item.(*transport.Obj)
		if it == nil || it.String("type") != "message" {
			continue
		}
		for _, c := range it.List("content") {
			content, _ := c.(*transport.Obj)
			if content != nil && content.String("type") == "output_text" {
				if v, ok := content.Get("text"); ok {
					if s, ok := v.(string); ok {
						sb.WriteString(s)
					}
				}
			}
		}
	}
	return sb.String()
}

func extractReasoningState(resp *transport.Obj) map[string]any {
	var items []any
	for _, item := range resp.List("output") {
		it, _ := item.(*transport.Obj)
		if it == nil || it.String("type") != "reasoning" || it.String("encrypted_content") == "" {
			continue
		}
		clone := it.Clone()
		clone.Delete("id")
		clone.Delete("status")
		items = append(items, clone)
	}
	if len(items) == 0 {
		return nil
	}
	return map[string]any{"reasoningItems": items}
}

func extractToolCalls(resp *transport.Obj) []llm.ToolCall {
	var calls []llm.ToolCall
	for _, item := range resp.List("output") {
		it, _ := item.(*transport.Obj)
		if it == nil || it.String("type") != "function_call" {
			continue
		}
		id := it.String("call_id")
		if id == "" {
			id = it.String("id")
		}
		name := it.String("name")
		if id == "" || name == "" {
			continue
		}
		calls = append(calls, llm.ToolCall{ID: id, Type: "function", Function: llm.FunctionCall{Name: name, Arguments: it.String("arguments")}})
	}
	return calls
}

func responsesUsage(resp *transport.Obj) *llm.Usage {
	u := resp.Object("usage")
	if u == nil {
		return nil
	}
	n := func(o *transport.Obj, key string) int {
		if o == nil {
			return 0
		}
		v, _ := o.Get(key)
		f, _ := v.(float64)
		return int(f)
	}
	return &llm.Usage{
		InputTokens: n(u, "input_tokens"), OutputTokens: n(u, "output_tokens"),
		ReasoningTokens:  n(u.Object("output_tokens_details"), "reasoning_tokens"),
		CachedReadTokens: n(u.Object("input_tokens_details"), "cached_tokens"),
	}
}
