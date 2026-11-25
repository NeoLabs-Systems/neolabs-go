package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/NeoLabs-Systems/neolabs-go/llm"
	"github.com/NeoLabs-Systems/neolabs-go/llm/anthropic"
	"github.com/NeoLabs-Systems/neolabs-go/llm/codex"
	"github.com/NeoLabs-Systems/neolabs-go/llm/google"
	"github.com/NeoLabs-Systems/neolabs-go/llm/transport"
)

// The scenarios in testdata/scenarios.json were recorded from the Node
// providers (testdata/gen/record.js) against a local fake upstream: the request
// each one sent and what it returned for a fixed reply. Every Go provider must
// send byte-identical request bodies and parse the replies to the same result.

type reply struct {
	Status      int               `json:"status"`
	ContentType string            `json:"contentType"`
	Headers     map[string]string `json:"headers"`
	Text        *string           `json:"text"`
	JSON        json.RawMessage   `json:"json"`
}

type recordedRequest struct {
	Method  string            `json:"method"`
	Path    string            `json:"path"`
	Body    string            `json:"body"`
	Headers map[string]string `json:"headers"`
}

type scenario struct {
	Name         string            `json:"name"`
	Provider     string            `json:"provider"`
	Method       string            `json:"method"`
	BasePath     string            `json:"basePath"`
	CodexBackend bool              `json:"codexBackend"`
	Config       map[string]any    `json:"config"`
	Options      scenarioOptions   `json:"options"`
	Messages     []json.RawMessage `json:"messages"`
	Tools        []llm.Tool        `json:"tools"`
	Upstream     json.RawMessage   `json:"upstream"`
	Result       *projection       `json:"result"`
	Events       []projection      `json:"events"`
	Error        *struct {
		Message string `json:"message"`
		Status  *int   `json:"status"`
	} `json:"error"`
	Requests []recordedRequest `json:"requests"`
}

type scenarioOptions struct {
	Model           string   `json:"model"`
	MaxTokens       int      `json:"maxTokens"`
	Temperature     *float64 `json:"temperature"`
	ReasoningEffort string   `json:"reasoningEffort"`
	ToolChoice      any      `json:"toolChoice"`
	Structured      bool     `json:"structured"`
	SessionID       string   `json:"sessionId"`
}

type projectedCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type projectedUsage struct {
	Input      int `json:"input"`
	Output     int `json:"output"`
	Reasoning  int `json:"reasoning"`
	CachedRead int `json:"cachedRead"`
	CacheWrite int `json:"cacheWrite"`
}

type projection struct {
	Type                  string          `json:"type,omitempty"`
	Content               string          `json:"content"`
	ToolCalls             []projectedCall `json:"toolCalls"`
	ThoughtSignatures     map[string]any  `json:"thoughtSignatures"`
	FinishReason          string          `json:"finishReason"`
	Truncated             bool            `json:"truncated"`
	Usage                 *projectedUsage `json:"usage"`
	Model                 string          `json:"model"`
	ReasoningState        map[string]any  `json:"reasoningState"`
	ProviderContentBlocks []any           `json:"providerContentBlocks"`
}

var generatedID = regexp.MustCompile(`^call_(ollama_)?\d+_\w+$`)

func project(typ, content string, calls []llm.ToolCall, finish string, truncated bool, usage *llm.Usage, model string, state map[string]any) *projection {
	p := &projection{Type: typ, Content: content, ToolCalls: []projectedCall{}, FinishReason: finish, Truncated: truncated, Model: model}
	for _, c := range calls {
		id := c.ID
		if generatedID.MatchString(id) {
			id = "call_generated"
		}
		p.ToolCalls = append(p.ToolCalls, projectedCall{ID: id, Name: c.Function.Name, Arguments: c.Function.Arguments})
	}
	if usage != nil {
		p.Usage = &projectedUsage{usage.InputTokens, usage.OutputTokens, usage.ReasoningTokens, usage.CachedReadTokens, usage.CacheWriteTokens}
	}
	rest := map[string]any{}
	for k, v := range state {
		switch k {
		case anthropic.ProviderContentBlocksKey:
			p.ProviderContentBlocks = canonical(v).([]any)
		case google.ThoughtSignaturesKey:
			sigs := canonical(v).(map[string]any)
			p.ThoughtSignatures = map[string]any{}
			for id, sig := range sigs {
				if generatedID.MatchString(id) {
					id = "call_generated"
				}
				p.ThoughtSignatures[id] = sig
			}
		default:
			rest[k] = canonical(v)
		}
	}
	if len(rest) > 0 {
		p.ReasoningState = rest
	}
	return p
}

// canonical round-trips a value through JSON so ordered objects and maps compare equal.
func canonical(v any) any {
	b, err := transport.Marshal(v)
	if err != nil {
		panic(err)
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		panic(err)
	}
	return out
}

func decodeMessages(t *testing.T, raw []json.RawMessage) []llm.Message {
	msgs := make([]llm.Message, len(raw))
	for i, r := range raw {
		if err := json.Unmarshal(r, &msgs[i]); err != nil {
			t.Fatal(err)
		}
		// Gemini thought signatures live on the tool call in the Node transcript;
		// Go carries them in the reasoning state.
		var generic struct {
			ToolCalls []struct {
				ID       string `json:"id"`
				Function struct {
					Signature string `json:"thought_signature"`
				} `json:"function"`
			} `json:"tool_calls"`
		}
		_ = json.Unmarshal(r, &generic)
		sigs := map[string]any{}
		for _, tc := range generic.ToolCalls {
			if tc.Function.Signature != "" {
				sigs[tc.ID] = tc.Function.Signature
			}
		}
		if len(sigs) > 0 {
			if msgs[i].ReasoningState == nil {
				msgs[i].ReasoningState = map[string]any{}
			}
			msgs[i].ReasoningState[google.ThoughtSignaturesKey] = sigs
		}
	}
	return msgs
}

type fakeUpstream struct {
	mu       sync.Mutex
	replies  []reply
	requests []recordedRequest
}

func newFake(t *testing.T, raw json.RawMessage) (*fakeUpstream, *httptest.Server) {
	f := &fakeUpstream{}
	if len(raw) > 0 && raw[0] == '[' {
		if err := json.Unmarshal(raw, &f.replies); err != nil {
			t.Fatal(err)
		}
	} else {
		var r reply
		if err := json.Unmarshal(raw, &r); err != nil {
			t.Fatal(err)
		}
		f.replies = []reply{r}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		defer f.mu.Unlock()
		headers := map[string]string{}
		for _, h := range []string{"authorization", "x-api-key", "x-goog-api-key", "anthropic-version", "anthropic-beta", "content-type", "chatgpt-account-id", "originator"} {
			if v := r.Header.Get(h); v != "" {
				headers[h] = v
			}
		}
		f.requests = append(f.requests, recordedRequest{Method: r.Method, Path: r.URL.RequestURI(), Body: string(body), Headers: headers})
		rep := f.replies[min(len(f.requests)-1, len(f.replies)-1)]
		for k, v := range rep.Headers {
			w.Header().Set(k, v)
		}
		ct := rep.ContentType
		if ct == "" {
			ct = "application/json"
		}
		w.Header().Set("Content-Type", ct)
		status := rep.Status
		if status == 0 {
			status = 200
		}
		w.WriteHeader(status)
		if rep.Text != nil {
			io.WriteString(w, *rep.Text)
		} else {
			var compact bytes.Buffer
			_ = json.Compact(&compact, rep.JSON)
			w.Write(compact.Bytes())
		}
	}))
	return f, srv
}

func buildProvider(t *testing.T, s scenario, srv *httptest.Server) llm.Provider {
	cfg := Config{Config: transport.Config{APIKey: "test-key", BaseURL: srv.URL + s.BasePath, HTTPClient: srv.Client()}}
	if key, ok := s.Config["apiKey"].(string); ok {
		cfg.APIKey = key
	}
	if s.Provider == "google" || s.Provider == "ollama" {
		cfg.BaseURL = srv.URL
	}
	if s.CodexBackend {
		return codex.New(codex.Config{Config: cfg.Config, ForceBackend: true})
	}
	p, err := New(s.Provider, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestNodeScenarios(t *testing.T) {
	raw, err := os.ReadFile("../testdata/scenarios.json")
	if err != nil {
		t.Fatal(err)
	}
	var scenarios []scenario
	if err := json.Unmarshal(raw, &scenarios); err != nil {
		t.Fatal(err)
	}
	if len(scenarios) < 30 {
		t.Fatalf("only %d scenarios recorded", len(scenarios))
	}
	for _, s := range scenarios {
		t.Run(s.Name, func(t *testing.T) {
			fake, srv := newFake(t, s.Upstream)
			defer srv.Close()
			p := buildProvider(t, s, srv)
			opt := llm.Options{Model: s.Options.Model, MaxTokens: s.Options.MaxTokens, Temperature: s.Options.Temperature, ReasoningEffort: s.Options.ReasoningEffort, Extra: map[string]any{}}
			if s.Options.ToolChoice != nil {
				opt.Extra["toolChoice"] = s.Options.ToolChoice
			}
			if s.Options.Structured {
				opt.Extra["structured"] = true
			}
			if s.Options.SessionID != "" {
				opt.Extra["sessionId"] = s.Options.SessionID
			}
			msgs := decodeMessages(t, s.Messages)

			var got *projection
			var events []projection
			var runErr error
			if s.Method == "chat" {
				res, err := p.Chat(context.Background(), msgs, s.Tools, opt)
				runErr = err
				if err == nil {
					got = project("", res.Content, res.ToolCalls, res.FinishReason, res.Truncated, res.Usage, res.Model, res.ReasoningState)
				}
			} else {
				for ev, err := range p.Stream(context.Background(), msgs, s.Tools, opt) {
					if err != nil {
						runErr = err
						break
					}
					events = append(events, *project(ev.Type, ev.Content, ev.ToolCalls, ev.FinishReason, ev.Truncated, ev.Usage, ev.Model, ev.ReasoningState))
				}
			}

			if s.Error != nil {
				if runErr == nil {
					t.Fatalf("expected error %q, got none", s.Error.Message)
				}
				if runErr.Error() != s.Error.Message {
					t.Errorf("error message\n got: %s\nwant: %s", runErr.Error(), s.Error.Message)
				}
				status := 0
				if e := llm.AsError(runErr); e != nil {
					status = e.Status
				}
				wantStatus := 0
				if s.Error.Status != nil {
					wantStatus = *s.Error.Status
				}
				if status != wantStatus {
					t.Errorf("error status = %d, want %d", status, wantStatus)
				}
			} else {
				if runErr != nil {
					t.Fatalf("unexpected error: %v", runErr)
				}
				if s.Method == "chat" {
					compare(t, "result", s.Result, got)
				} else {
					compare(t, "events", s.Events, events)
				}
			}

			if s.Name == "claude-code chat adds the Claude Code system prompt" {
				checkClaudeCodeRequest(t, fake)
				return
			}
			compareRequests(t, s.Requests, fake.requests)
		})
	}
}

func compare(t *testing.T, what string, want, got any) {
	t.Helper()
	w, g := canonical(want), canonical(got)
	if !reflect.DeepEqual(w, g) {
		wb, _ := json.MarshalIndent(w, "", " ")
		gb, _ := json.MarshalIndent(g, "", " ")
		t.Errorf("%s differs from Node\n got: %s\nwant: %s", what, gb, wb)
	}
}

func compareRequests(t *testing.T, want, got []recordedRequest) {
	t.Helper()
	if len(want) != len(got) {
		t.Fatalf("sent %d requests, Node sent %d", len(got), len(want))
	}
	for i := range want {
		w, g := want[i], got[i]
		if w.Method != g.Method || w.Path != g.Path {
			t.Errorf("request %d: %s %s, want %s %s", i, g.Method, g.Path, w.Method, w.Path)
		}
		if w.Body != g.Body {
			t.Errorf("request %d body differs from Node\n got: %s\nwant: %s", i, g.Body, w.Body)
		}
		for k, v := range w.Headers {
			if g.Headers[k] != v {
				t.Errorf("request %d header %s = %q, want %q", i, k, g.Headers[k], v)
			}
		}
	}
}

// Node builds the Claude Code system block with an array where the text belongs
// (the API would reject it); Go sends the Claude Code prompt followed by the
// caller's system blocks, which is what the code means to do.
func checkClaudeCodeRequest(t *testing.T, f *fakeUpstream) {
	t.Helper()
	if len(f.requests) != 1 {
		t.Fatalf("requests = %d", len(f.requests))
	}
	want := `{"model":"claude-sonnet-4-5","max_tokens":16384,"messages":[{"role":"user","content":[{"type":"text","text":"What is the weather in Berlin?","cache_control":{"type":"ephemeral"}}]}],` +
		`"system":[{"type":"text","text":"You are Claude Code, Anthropic's official CLI for Claude.","cache_control":{"type":"ephemeral"}},{"type":"text","text":"You are a precise assistant."}]}`
	if f.requests[0].Body != want {
		t.Errorf("claude-code body\n got: %s\nwant: %s", f.requests[0].Body, want)
	}
	h := f.requests[0].Headers
	if h["authorization"] != "Bearer oauth-token" || !strings.Contains(h["anthropic-beta"], "oauth-2025-04-20") {
		t.Errorf("claude-code headers: %v", h)
	}
}
