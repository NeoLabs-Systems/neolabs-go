// Package google serves Gemini through the Generative Language REST API
// (generateContent and streamGenerateContent).
package google

import (
	"fmt"
	"strings"

	"github.com/NeoLabs-Systems/neolabs-go/llm"
	"github.com/NeoLabs-Systems/neolabs-go/llm/transport"
)

// ThoughtSignaturesKey names the entry of Result.ReasoningState (and of the
// Message.ReasoningState it is sent back in) that maps a tool call id to the
// thought signature Gemini issued with it. Thinking models reject a functionCall
// that comes back without it.
const ThoughtSignaturesKey = "thoughtSignatures"

// thinkingBudgets are Google's own effort mappings for Gemini 2.x, which only
// takes a token budget; later models take a level. MINIMAL is left out because
// Pro models reject it.
var thinkingBudgets = map[string]int{"low": 1024, "medium": 8192, "high": 24576}

func (p *Provider) thinkingConfig(model, requested string) *transport.Obj {
	effort := strings.ToLower(strings.TrimSpace(requested))
	budget, ok := thinkingBudgets[effort]
	// Gemma models served by the same API think but reject both settings, so
	// only Gemini models get one.
	if !strings.HasPrefix(model, "gemini-") || !p.cache.isThinking(model) || !ok {
		return nil
	}
	if strings.HasPrefix(model, "gemini-2.") {
		return transport.NewObj().Set("thinkingBudget", budget)
	}
	return transport.NewObj().Set("thinkingLevel", strings.ToUpper(effort))
}

func formatTools(tools []llm.Tool) []any {
	decls := []any{}
	for _, t := range tools {
		name := strings.TrimSpace(t.Name)
		if name == "" {
			continue
		}
		d := transport.NewObj().Set("name", name)
		if t.Description != "" {
			d.Set("description", t.Description)
		}
		if t.Parameters == nil {
			d.Set("parametersJsonSchema", transport.NewObj().Set("type", "object").Set("properties", transport.NewObj()))
		} else {
			d.Set("parametersJsonSchema", t.Parameters)
		}
		decls = append(decls, d)
	}
	return []any{transport.NewObj().Set("functionDeclarations", decls)}
}

// parseToolArguments reads tool arguments as an object, keeping key order;
// anything else becomes {}.
func parseToolArguments(raw string) *transport.Obj {
	if strings.TrimSpace(raw) == "" {
		raw = "{}"
	}
	v, err := transport.ParseOrdered([]byte(raw))
	if err != nil {
		return transport.NewObj()
	}
	if o, ok := v.(*transport.Obj); ok {
		return o
	}
	return transport.NewObj()
}

func contentText(c any) string {
	s, _ := c.(string)
	return s
}

// contentParts maps a message's text and image parts to Gemini parts.
func contentParts(content any) []any {
	list, ok := content.([]any)
	if !ok {
		return []any{transport.NewObj().Set("text", contentText(content))}
	}
	var parts []any
	for _, part := range list {
		if text, isText := llm.TextPart(part); isText {
			parts = append(parts, transport.NewObj().Set("text", text))
		} else if mimeType, data, isImage := llm.ImagePart(part); isImage {
			parts = append(parts, transport.NewObj().Set("inlineData", transport.NewObj().Set("mimeType", mimeType).Set("data", data)))
		}
	}
	return parts
}

type turn struct {
	role  string
	parts []any
}

// convertMessages builds the system instruction and the alternating
// user/model turns Gemini requires.
func (p *Provider) convertMessages(msgs []llm.Message, model string) (string, []turn) {
	var system strings.Builder
	var history []turn
	requiresSignatures := p.cache.isThinking(model)
	// Calls another model made earlier in the run carry no thought signature, so
	// for thinking models they and their results are passed as text.
	unsigned := map[string]bool{}
	for _, m := range msgs {
		switch {
		case m.Role == "system":
			if system.Len() > 0 {
				system.WriteString("\n\n")
			}
			system.WriteString(llm.ContentText(m.Content))
		case m.Role == "tool" && unsigned[m.ToolCallID]:
			name := m.Name
			if name == "" {
				name = "tool"
			}
			history = append(history, turn{"user", []any{transport.NewObj().Set("text", fmt.Sprintf("[Result of %s]\n%s", name, contentText(m.Content)))}})
		case m.Role == "tool":
			name := m.Name
			if name == "" {
				name = "tool"
			}
			fr := transport.NewObj().Set("name", name).Set("response", transport.NewObj().Set("result", m.Content))
			if m.ToolCallID != "" {
				fr.Set("id", m.ToolCallID)
			}
			history = append(history, turn{"user", []any{transport.NewObj().Set("functionResponse", fr)}})
		case m.Role == "assistant" && m.ToolCalls != nil:
			var parts []any
			if text := contentText(m.Content); text != "" {
				parts = append(parts, transport.NewObj().Set("text", text))
			}
			signatures, _ := m.ReasoningState[ThoughtSignaturesKey].(map[string]any)
			signed := false
			for _, tc := range m.ToolCalls {
				if s, _ := signatures[tc.ID].(string); s != "" {
					signed = true
				}
			}
			// Gemini signs only the first call of a parallel group, so a turn with
			// any signature is its own.
			skipCalls := requiresSignatures && !signed
			for _, tc := range m.ToolCalls {
				if tc.Function.Name == "" {
					continue
				}
				args := parseToolArguments(tc.Function.Arguments)
				if skipCalls {
					if tc.ID != "" {
						unsigned[tc.ID] = true
					}
					encoded, _ := transport.Marshal(args)
					parts = append(parts, transport.NewObj().Set("text", fmt.Sprintf("[Called %s with %s]", tc.Function.Name, encoded)))
					continue
				}
				call := transport.NewObj().Set("args", args)
				if tc.ID != "" {
					call.Set("id", tc.ID)
				}
				call.Set("name", tc.Function.Name)
				part := transport.NewObj().Set("functionCall", call)
				if s, _ := signatures[tc.ID].(string); s != "" {
					part.Set("thoughtSignature", s)
				}
				parts = append(parts, part)
			}
			history = append(history, turn{"model", parts})
		default:
			role := "user"
			if m.Role == "assistant" {
				role = "model"
			}
			history = append(history, turn{role, contentParts(m.Content)})
		}
	}

	var merged []turn
	for _, t := range history {
		if n := len(merged); n > 0 && merged[n-1].role == t.role {
			merged[n-1].parts = append(merged[n-1].parts, t.parts...)
			continue
		}
		merged = append(merged, turn{t.role, append([]any(nil), t.parts...)})
	}
	// Structured helpers can consist entirely of system instructions. Gemini
	// still requires a user turn to trigger generation.
	if len(merged) == 0 || merged[len(merged)-1].role == "model" {
		merged = append(merged, turn{"user", []any{transport.NewObj().Set("text", "Provide the requested response using the system instructions.")}})
	}
	if merged[0].role != "user" {
		merged = append([]turn{{"user", []any{transport.NewObj().Set("text", "[Conversation Resumed]")}}}, merged...)
	}
	return system.String(), merged
}

func (p *Provider) requestBody(model string, msgs []llm.Message, tools []llm.Tool, opt llm.Options) *transport.Obj {
	system, history := p.convertMessages(msgs, model)
	contents := make([]any, len(history))
	for i, t := range history {
		contents[i] = transport.NewObj().Set("parts", t.parts).Set("role", t.role)
	}
	body := transport.NewObj().Set("contents", contents)
	if system != "" {
		body.Set("systemInstruction", transport.NewObj().Set("parts", []any{transport.NewObj().Set("text", system)}).Set("role", "user"))
	}
	if len(tools) > 0 {
		body.Set("tools", formatTools(tools))
	}
	gen := transport.NewObj()
	if opt.MaxTokens > 0 {
		gen.Set("maxOutputTokens", opt.MaxTokens)
	}
	if opt.JSON {
		gen.Set("responseMimeType", "application/json")
	}
	if thinking := p.thinkingConfig(model, opt.ReasoningEffort); thinking != nil {
		gen.Set("thinkingConfig", thinking)
	}
	body.Set("generationConfig", gen)
	return body
}
