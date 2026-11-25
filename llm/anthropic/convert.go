// Package anthropic serves the Anthropic Messages API: Anthropic itself, any
// service with an Anthropic-compatible endpoint (MiniMax) and Claude Code
// subscriptions through OAuth.
package anthropic

import (
	"encoding/json"
	"strings"

	"github.com/NeoLabs-Systems/neolabs-go/llm"
	"github.com/NeoLabs-Systems/neolabs-go/llm/transport"
)

func cacheBreakpoint() *transport.Obj { return transport.NewObj().Set("type", "ephemeral") }

// blockType reads the type of a content block held as an object or a map.
func blockType(b any) string {
	switch t := b.(type) {
	case *transport.Obj:
		return t.String("type")
	case map[string]any:
		s, _ := t["type"].(string)
		return s
	}
	return ""
}

func blockText(b any) string {
	switch t := b.(type) {
	case *transport.Obj:
		return t.String("text")
	case map[string]any:
		s, _ := t["text"].(string)
		return s
	}
	return ""
}

// withCache copies a block and marks it as a cache breakpoint.
func withCache(b any) any {
	switch t := b.(type) {
	case *transport.Obj:
		return t.Clone().Set("cache_control", cacheBreakpoint())
	case map[string]any:
		return transport.FromMap(t).Set("cache_control", cacheBreakpoint())
	}
	return b
}

// applyCacheBreakpoints marks tools, then the stable system block, then the
// newest turn: tools rarely change, the stable block holds across runs, and the
// last breakpoint moves forward with the run so each turn reads what the one
// before it wrote. Thinking blocks cannot carry a breakpoint, and a prompt below
// the model's minimum simply is not cached.
func applyCacheBreakpoints(params *transport.Obj) {
	if tools, _ := params.Get("tools"); tools != nil {
		list := tools.([]any)
		if len(list) > 0 {
			last := len(list) - 1
			list[last] = withCache(list[last])
		}
	}
	if system, _ := params.Get("system"); system != nil {
		list := system.([]any)
		if len(list) > 0 {
			list[0] = withCache(list[0])
		}
	}
	msgsAny, _ := params.Get("messages")
	msgs, _ := msgsAny.([]any)
	if len(msgs) == 0 {
		return
	}
	last := msgs[len(msgs)-1].(*transport.Obj)
	var blocks []any
	content, has := last.Get("content")
	if list, ok := content.([]any); ok {
		blocks = list
	} else {
		text := ""
		if s, ok := content.(string); ok {
			text = s
		} else if has && content != nil {
			text = jsString(content)
		}
		blocks = []any{transport.NewObj().Set("type", "text").Set("text", text)}
	}
	for i := len(blocks) - 1; i >= 0; i-- {
		typ := blockType(blocks[i])
		if typ == "thinking" || typ == "redacted_thinking" {
			continue
		}
		if typ == "text" && blockText(blocks[i]) == "" {
			continue
		}
		next := append([]any(nil), blocks...)
		next[i] = withCache(blocks[i])
		msgs[len(msgs)-1] = last.Clone().Set("content", next)
		return
	}
}

func jsString(v any) string {
	b, err := transport.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

// formatTools lists the tools in Anthropic's shape.
func formatTools(tools []llm.Tool) []any {
	out := make([]any, 0, len(tools))
	for _, t := range tools {
		name := strings.TrimSpace(t.Name)
		if name == "" {
			continue
		}
		o := transport.NewObj().Set("name", name)
		if t.Description != "" {
			o.Set("description", t.Description)
		}
		if t.Parameters == nil {
			o.Set("input_schema", transport.NewObj().Set("type", "object").Set("properties", transport.NewObj()))
		} else {
			o.Set("input_schema", t.Parameters)
		}
		out = append(out, o)
	}
	return out
}

// normalizeContentBlocks keeps the block kinds the API takes back (thinking,
// redacted thinking, text, tool use) with only their API fields.
func normalizeContentBlocks(blocks []any) []any {
	out := []any{}
	for _, b := range blocks {
		m := toMap(b)
		switch blockTypeOf(m) {
		case "thinking":
			o := transport.NewObj().Set("type", "thinking").Set("thinking", str(m["thinking"]))
			if sig := str(m["signature"]); sig != "" {
				o.Set("signature", sig)
			}
			out = append(out, o)
		case "redacted_thinking":
			out = append(out, transport.NewObj().Set("type", "redacted_thinking").Set("data", m["data"]))
		case "text":
			out = append(out, transport.NewObj().Set("type", "text").Set("text", str(m["text"])))
		case "tool_use":
			input := m["input"]
			if input == nil || input == false || input == "" {
				input = transport.NewObj()
			}
			out = append(out, transport.NewObj().Set("type", "tool_use").Set("id", m["id"]).Set("name", m["name"]).Set("input", input))
		}
	}
	return out
}

func toMap(b any) map[string]any {
	switch t := b.(type) {
	case map[string]any:
		return t
	case *transport.Obj:
		m := map[string]any{}
		for _, k := range t.Keys() {
			m[k], _ = t.Get(k)
		}
		return m
	}
	return map[string]any{}
}

func blockTypeOf(m map[string]any) string { return str(m["type"]) }

func str(v any) string {
	s, _ := v.(string)
	return s
}

// providerBlocks returns the content blocks a message carries from an earlier
// turn: its own field, or the reasoning state the answer came back with.
func providerBlocks(m llm.Message) []any {
	if len(m.ProviderContentBlocks) > 0 {
		return m.ProviderContentBlocks
	}
	if blocks, ok := m.ReasoningState[ProviderContentBlocksKey].([]any); ok {
		return blocks
	}
	return nil
}

// ProviderContentBlocksKey names the entry of Result.ReasoningState (and of the
// Message.ReasoningState it is sent back in) that holds the assistant's content
// blocks, thinking included, exactly as Anthropic returned them.
const ProviderContentBlocksKey = "providerContentBlocks"

// convertMessages splits the transcript into the system blocks and API messages.
func convertMessages(msgs []llm.Message) (system []any, messages []any) {
	// A note added mid-run must not change the system block, or every cached turn
	// after it would be lost. Later notes travel as text in the conversation.
	toolTurnSeen := false
	for _, msg := range msgs {
		if msg.Role == "system" {
			text, _ := msg.Content.(string)
			block := transport.NewObj().Set("type", "text").Set("text", text)
			if toolTurnSeen {
				messages = append(messages, transport.NewObj().Set("role", "user").Set("content", []any{block}))
				continue
			}
			system = append(system, block)
			continue
		}
		if msg.Role == "assistant" && len(msg.ToolCalls) > 0 {
			toolTurnSeen = true
		}
		if msg.Role == "tool" {
			result := transport.NewObj().Set("type", "tool_result").Set("tool_use_id", msg.ToolCallID)
			switch c := msg.Content.(type) {
			case nil:
			case string:
				result.Set("content", c)
			default:
				result.Set("content", jsString(c))
			}
			messages = append(messages, transport.NewObj().Set("role", "user").Set("content", []any{result}))
			continue
		}
		if msg.Role == "assistant" && msg.ToolCalls != nil {
			if blocks := providerBlocks(msg); len(blocks) > 0 {
				messages = append(messages, transport.NewObj().Set("role", "assistant").Set("content", normalizeContentBlocks(blocks)))
				continue
			}
			content := []any{}
			if text, _ := msg.Content.(string); text != "" {
				content = append(content, transport.NewObj().Set("type", "text").Set("text", text))
			}
			for _, tc := range msg.ToolCalls {
				if tc.Function.Name == "" {
					continue
				}
				content = append(content, transport.NewObj().
					Set("type", "tool_use").Set("id", tc.ID).Set("name", tc.Function.Name).Set("input", parseInput(tc.Function.Arguments)))
			}
			messages = append(messages, transport.NewObj().Set("role", "assistant").Set("content", content))
			continue
		}
		role := "user"
		if msg.Role == "assistant" {
			role = "assistant"
		}
		o := transport.NewObj().Set("role", role)
		if msg.Content != nil {
			o.Set("content", msg.Content)
		}
		messages = append(messages, o)
	}
	return system, messages
}

// parseInput reads a tool call's arguments as the model produced them, keeping
// key order (the prompt cache is sensitive to it); malformed text becomes {}.
func parseInput(arguments string) any {
	if strings.TrimSpace(arguments) == "" {
		arguments = "{}"
	}
	v, err := transport.ParseOrdered([]byte(arguments))
	if err != nil {
		return transport.NewObj()
	}
	return v
}

// usageOf is promptTokensOf: Anthropic's input_tokens leaves out what the cache
// served, every other provider counts it, so this is the one place that makes
// the numbers mean the same thing.
func promptTokens(u map[string]any) int {
	return num(u["input_tokens"]) + num(u["cache_read_input_tokens"]) + num(u["cache_creation_input_tokens"])
}

func num(v any) int {
	switch t := v.(type) {
	case float64:
		return int(t)
	case json.Number:
		f, _ := t.Float64()
		return int(f)
	}
	return 0
}
