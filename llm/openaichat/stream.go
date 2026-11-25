package openaichat

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"

	"github.com/NeoLabs-Systems/neolabs-go/llm"
	"github.com/NeoLabs-Systems/neolabs-go/llm/transport"
)

var errStopped = errors.New("stream consumer stopped")

type toolDelta struct {
	Index    *float64 `json:"index"`
	ID       string   `json:"id"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type streamChunk struct {
	Model   string          `json:"model"`
	Usage   map[string]any  `json:"usage"`
	Error   json.RawMessage `json:"error"`
	Choices []struct {
		Delta struct {
			Content          any              `json:"content"`
			ReasoningContent any              `json:"reasoning_content"`
			Reasoning        any              `json:"reasoning"`
			ReasoningDetails []map[string]any `json:"reasoning_details"`
			ToolCalls        []toolDelta      `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
}

// mergeReasoningDetails joins the pieces OpenRouter streams for one block in
// place; the sequence has to go back exactly as it came.
func mergeReasoningDetails(details []map[string]any, incoming []map[string]any) []map[string]any {
	for _, piece := range incoming {
		index := len(details)
		if i, ok := piece["index"].(float64); ok && i == math.Trunc(i) && i >= 0 {
			index = int(i)
		}
		for len(details) <= index {
			details = append(details, nil)
		}
		current := details[index]
		if current == nil {
			cp := make(map[string]any, len(piece))
			for k, v := range piece {
				cp[k] = v
			}
			details[index] = cp
			continue
		}
		for _, field := range []string{"text", "summary", "data"} {
			if s, ok := piece[field].(string); ok {
				prev, _ := current[field].(string)
				current[field] = prev + s
			}
		}
		if sig, ok := piece["signature"]; ok && truthy(sig) {
			current["signature"] = sig
		}
		if f, ok := piece["format"]; ok && truthy(f) {
			current["format"] = f
		}
	}
	return details
}

func truthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case string:
		return t != ""
	case bool:
		return t
	case float64:
		return t != 0
	}
	return true
}

// declaredKeys maps each tool to the argument keys it declares (nil: any key).
func declaredKeys(tools []llm.Tool) map[string]map[string]bool {
	out := map[string]map[string]bool{}
	for _, t := range tools {
		props, _ := t.Parameters["properties"].(map[string]any)
		if props != nil && t.Parameters["additionalProperties"] != true {
			keys := make(map[string]bool, len(props))
			for k := range props {
				keys[k] = true
			}
			out[t.Name] = keys
		} else {
			out[t.Name] = nil
		}
	}
	return out
}

func (p *Provider) readStream(ctx context.Context, body io.Reader, tools []llm.Tool, emit func(llm.Event) bool) error {
	contentGuard := llm.NewStreamGuard()
	known := declaredKeys(tools)
	var (
		calls          []*llm.ToolCall
		argGuards      []*llm.StreamGuard
		argJSON        []*llm.JSONPrefixTracker
		content        string
		reasoningText  string
		reasoningPlain string
		details        []map[string]any
		finish         string
		usage          *llm.Usage
		served         string
	)
	check := func(v *llm.GuardVerdict, toolName string) error {
		if v == nil {
			return nil
		}
		return llm.NewDegenerateOutputError(p.name, *v, toolName)
	}

	err := transport.ReadSSE(body, func(ev transport.SSEEvent) error {
		if ev.Data == "[DONE]" {
			return io.EOF
		}
		var c streamChunk
		if err := json.Unmarshal([]byte(ev.Data), &c); err != nil {
			return &llm.Error{Provider: p.name, Message: "Could not parse stream chunk as JSON"}
		}
		if len(c.Error) > 0 && string(c.Error) != "null" {
			return llm.StatusError(p.name, 0, nil, c.Error, true)
		}
		if c.Usage != nil {
			usage = usageFrom(c.Usage)
		}
		if c.Model != "" {
			served = c.Model
		}
		if len(c.Choices) == 0 {
			return nil
		}
		choice := c.Choices[0]
		delta := choice.Delta
		if text := textField(delta.Content); text != "" {
			content += text
			if err := check(contentGuard.Feed(text), ""); err != nil {
				return err
			}
			if !emit(llm.Event{Type: llm.EventContent, Content: text}) {
				return errStopped
			}
		}
		reasoningText += textField(delta.ReasoningContent)
		reasoningPlain += textField(delta.Reasoning)
		if len(delta.ReasoningDetails) > 0 {
			details = mergeReasoningDetails(details, delta.ReasoningDetails)
		}
		for _, tc := range delta.ToolCalls {
			index := len(calls)
			if tc.Index != nil && *tc.Index == math.Trunc(*tc.Index) && *tc.Index >= 0 {
				index = int(*tc.Index)
			}
			for len(calls) <= index {
				calls = append(calls, nil)
				argGuards = append(argGuards, nil)
				argJSON = append(argJSON, nil)
			}
			if calls[index] == nil {
				call := &llm.ToolCall{Type: "function", ID: tc.ID}
				calls[index] = call
				argGuards[index] = llm.NewStreamGuard()
				argJSON[index] = llm.NewJSONPrefixTracker(func(key string) bool {
					keys, declared := known[call.Function.Name]
					return !declared || keys == nil || keys[key]
				})
			}
			call := calls[index]
			if tc.ID != "" {
				call.ID = tc.ID
			}
			// Names arrive once, in pieces, or repeated on every delta depending
			// on the endpoint; append only what is not already there.
			if n := tc.Function.Name; n != "" && !hasSuffix(call.Function.Name, n) {
				call.Function.Name += n
			}
			if a := tc.Function.Arguments; a != "" {
				call.Function.Arguments += a
				if !argJSON[index].Feed(a) {
					verdict := llm.GuardVerdict{Reason: argJSON[index].Reason(), Key: argJSON[index].Key(), Bytes: utf16Len(call.Function.Arguments)}
					return check(&verdict, call.Function.Name)
				}
				if err := check(argGuards[index].Feed(a), call.Function.Name); err != nil {
					return err
				}
			}
		}
		if choice.FinishReason != nil && *choice.FinishReason != "" {
			finish = *choice.FinishReason
		}
		return nil
	})
	if err != nil && err != io.EOF {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, errStopped) {
			return err
		}
		return streamFailure(p.name, err)
	}

	var final []llm.ToolCall
	for _, c := range calls {
		if c != nil {
			final = append(final, *c)
		}
	}
	ev := llm.Event{
		Type: llm.EventDone, Content: content, ToolCalls: final, FinishReason: finish,
		// Output stopped at the token limit rather than because the model was
		// done. Callers must not treat that as a finished answer.
		Truncated: finish == "length", Usage: usage, Model: served,
	}
	if len(final) > 0 {
		ev.Type = llm.EventToolCalls
		ev.ReasoningState = reasoningState(reasoningText, reasoningPlain, details)
	}
	if !emit(ev) {
		return errStopped
	}
	return nil
}

func hasSuffix(s, suffix string) bool {
	return len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix
}

func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		if r >= 0x10000 {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// streamFailure leaves model-side errors as they are and turns a broken
// connection into a retryable one.
func streamFailure(provider string, err error) error {
	var le *llm.Error
	var dg *llm.DegenerateOutputError
	if errors.As(err, &le) || errors.As(err, &dg) {
		return err
	}
	return &llm.Error{Provider: provider, Message: "terminated", Retryable: true}
}
