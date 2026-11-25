package providers

import (
	"context"
	"strings"
	"testing"

	"github.com/NeoLabs-Systems/neolabs-go/llm"
)

func lastBody(rec *recorder) string {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return rec.body[len(rec.body)-1]
}

func TestOpenRouterSendsEffortOnlyWhereTheCatalogAllowsIt(t *testing.T) {
	srv, rec := serve(t, map[string]route{
		"/api/v1/models":           {body: `{"data":[{"id":"a/think","reasoning":{"default_enabled":true,"supported_efforts":["low","high"]}},{"id":"b/optional","reasoning":{"default_enabled":false}},{"id":"c/mandatory","reasoning":{"mandatory":true}}]}`},
		"/api/v1/chat/completions": {body: chatOK},
	})
	p, _ := New("openrouter", cfgFor(srv, "/api/v1"))
	if _, err := p.Models(context.Background()); err != nil {
		t.Fatal(err)
	}
	for model, tc := range map[string]struct{ effort, want string }{
		"a/think":     {"high", `"reasoning":{"effort":"high"}`},
		"z/unknown":   {"high", ""}, // not in the catalog
		"b/optional":  {"high", ""}, // would switch thinking on
		"c/mandatory": {"MEDIUM", `"reasoning":{"effort":"medium"}`},
	} {
		if _, err := p.Chat(context.Background(), nil, nil, llm.Options{Model: model, ReasoningEffort: tc.effort}); err != nil {
			t.Fatal(err)
		}
		body := lastBody(rec)
		if (tc.want == "") == strings.Contains(body, `"reasoning"`) || (tc.want != "" && !strings.Contains(body, tc.want)) {
			t.Errorf("%s effort %s: body %s", model, tc.effort, body)
		}
	}
	// "medium" is not among a/think's supported efforts
	if _, err := p.Chat(context.Background(), nil, nil, llm.Options{Model: "a/think", ReasoningEffort: "medium"}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(lastBody(rec), `"reasoning"`) {
		t.Errorf("unsupported effort sent: %s", lastBody(rec))
	}
}

func TestOpenAIFamilyMessagesCarryOnlyKnownReasoningFields(t *testing.T) {
	srv, rec := serve(t, map[string]route{"/v1/chat/completions": {body: chatOK}})
	p, _ := New("openai-compatible", cfgFor(srv, "/v1"))
	msgs := []llm.Message{
		{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "c1", Type: "function", Function: llm.FunctionCall{Name: "f", Arguments: "{}"}}},
			ReasoningState:        map[string]any{"reasoning_content": "thought", "providerContentBlocks": []any{"x"}, "thoughtSignatures": map[string]any{"c1": "s"}, "reasoningItems": []any{"y"}},
			ProviderContentBlocks: []any{"never sent"}},
		{Role: "tool", ToolCallID: "c1", Name: "f", Content: "ok"},
	}
	if _, err := p.Chat(context.Background(), msgs, nil, llm.Options{Model: "m"}); err != nil {
		t.Fatal(err)
	}
	want := `{"model":"m","messages":[{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}],"reasoning_content":"thought"},{"role":"tool","name":"f","tool_call_id":"c1","content":"ok"}],"max_tokens":16384}`
	// content comes right after role when present
	want = strings.Replace(want, `{"role":"tool","name":"f","tool_call_id":"c1","content":"ok"}`, `{"role":"tool","content":"ok","name":"f","tool_call_id":"c1"}`, 1)
	if got := lastBody(rec); got != want {
		t.Errorf("body\n got: %s\nwant: %s", got, want)
	}
}

func TestAnthropicStreamFailures(t *testing.T) {
	for name, tc := range map[string]struct {
		body      string
		want      string
		transient bool
	}{
		"overloaded": {"event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}\n\n",
			`{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`, true},
		"cut off": {"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":1}}}\n\n", "terminated", true},
	} {
		srv, _ := serve(t, map[string]route{"/v1/messages": {ctype: "text/event-stream", body: tc.body}})
		p, _ := New("anthropic", cfgFor(srv, ""))
		var err error
		for _, e := range p.Stream(context.Background(), []llm.Message{{Role: "user", Content: "x"}}, nil, llm.Options{Model: "m"}) {
			err = e
		}
		if err == nil || err.Error() != tc.want || llm.IsTransient(err) != tc.transient {
			t.Errorf("%s: %v (transient %v)", name, err, llm.IsTransient(err))
		}
	}
}

func TestOllamaStreamProblems(t *testing.T) {
	tags := `{"models":[{"name":"m:latest"}]}`
	for name, tc := range map[string]struct{ stream, want string }{
		"malformed":     {"{not json}\n", "Ollama /api/chat returned malformed streaming JSON."},
		"error body":    {`{"error":"model crashed"}` + "\n", "Ollama request failed: model crashed"},
		"nameless call": {`{"message":{"tool_calls":[{"function":{"arguments":{}}}]},"done":false}` + "\n", "Ollama /api/chat returned malformed streaming JSON."},
		"no marker":     {`{"message":{"content":"x"},"done":false}` + "\n", "Ollama stream ended before sending a completion marker."},
	} {
		srv, _ := serve(t, map[string]route{"/api/tags": {body: tags}, "/api/chat": {ctype: "application/x-ndjson", body: tc.stream}})
		p, _ := New("ollama", cfgFor(srv, ""))
		var err error
		for _, e := range p.Stream(context.Background(), nil, nil, llm.Options{Model: "m"}) {
			err = e
		}
		if err == nil || err.Error() != tc.want {
			t.Errorf("%s: %v", name, err)
		}
	}
	// a tool call with string arguments passes them through
	srv, _ := serve(t, map[string]route{"/api/tags": {body: tags}, "/api/chat": {body: `{"message":{"tool_calls":[{"function":{"name":"f","arguments":"{\"a\":1}"}}]},"done":true}` + "\n"}})
	p, _ := New("ollama", cfgFor(srv, ""))
	var final llm.Event
	for e, err := range p.Stream(context.Background(), nil, nil, llm.Options{Model: "m"}) {
		if err != nil {
			t.Fatal(err)
		}
		final = e
	}
	if final.Type != llm.EventDone || final.FinishReason != "tool_calls" || final.ToolCalls[0].Function.Arguments != `{"a":1}` {
		t.Errorf("final = %+v", final)
	}
}

func TestCodexFailedResponseIsAnError(t *testing.T) {
	body := "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"par\"}\n\n" +
		"event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"error\":{\"message\":\"context length exceeded\"}}}\n\n"
	srv, _ := serve(t, map[string]route{"/v1/responses": {ctype: "text/event-stream", body: body}})
	p, _ := New("openai-codex", cfgFor(srv, "/v1"))
	var err error
	for _, e := range p.Stream(context.Background(), nil, nil, llm.Options{Model: "gpt-5"}) {
		err = e
	}
	if err == nil || err.Error() != "context length exceeded" {
		t.Errorf("err = %v", err)
	}
}

func TestOpenAIChatMatchesNodeOnTruncationAndErrorsOnEmptyChoices(t *testing.T) {
	srv, _ := serve(t, map[string]route{"/v1/chat/completions": {body: `{"choices":[{"finish_reason":"length","message":{"content":"cut"}}]}`}})
	p, _ := New("openai", cfgFor(srv, "/v1"))
	res, err := p.Chat(context.Background(), nil, nil, llm.Options{Model: "gpt-4.1"})
	if err != nil || res.Truncated || res.FinishReason != "length" {
		t.Errorf("openai chat = %+v, %v (Node's openai chat never reports truncation)", res, err)
	}
	c, _ := New("openai-compatible", cfgFor(srv, "/v1"))
	if res, _ := c.Chat(context.Background(), nil, nil, llm.Options{Model: "m"}); !res.Truncated {
		t.Error("custom endpoints report truncation")
	}
	srv2, _ := serve(t, map[string]route{"/v1/chat/completions": {body: `{"choices":[]}`}})
	c, _ = New("openai-compatible", cfgFor(srv2, "/v1"))
	if _, err := c.Chat(context.Background(), nil, nil, llm.Options{Model: "m"}); err == nil || err.Error() != "Provider 'openai-compatible' returned no choices in the response" {
		t.Errorf("err = %v", err)
	}
}

func TestAnalyzeImage(t *testing.T) {
	srv, rec := serve(t, map[string]route{"/v1/chat/completions": {body: `{"choices":[{"message":{"content":"a cat"}}]}`}})
	p, _ := New("openai", cfgFor(srv, "/v1"))
	text, err := p.(llm.ImageAnalyzer).AnalyzeImage(context.Background(), llm.ImageRequest{Model: "gpt-4.1", ImageB64: "AAAA"})
	if err != nil || text != "a cat" {
		t.Fatalf("%q %v", text, err)
	}
	want := `{"model":"gpt-4.1","max_tokens":4096,"messages":[{"role":"user","content":[{"type":"text","text":"Describe this image in detail."},{"type":"image_url","image_url":{"url":"data:image/jpeg;base64,AAAA"}}]}]}`
	if got := lastBody(rec); got != want {
		t.Errorf("body = %s", got)
	}
	n, _ := New("nvidia", cfgFor(srv, "/v1"))
	if _, err := n.(llm.ImageAnalyzer).AnalyzeImage(context.Background(), llm.ImageRequest{Model: "m"}); err == nil || err.Error() != "Provider 'nvidia' does not support image analysis" {
		t.Errorf("nvidia: %v", err)
	}
}
