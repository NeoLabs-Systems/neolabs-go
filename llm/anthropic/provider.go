package anthropic

import (
	"context"
	"encoding/json"
	"io"
	"iter"
	"net/http"
	"strings"

	"github.com/NeoLabs-Systems/neolabs-go/llm"
	"github.com/NeoLabs-Systems/neolabs-go/llm/openaichat"
	"github.com/NeoLabs-Systems/neolabs-go/llm/transport"
)

const (
	defaultBaseURL = "https://api.anthropic.com"
	apiVersion     = "2023-06-01"
)

// Provider is one configured Anthropic Messages API client.
type Provider struct {
	hc      *http.Client
	baseURL string
	// auth returns the credential headers for a request.
	auth    func() map[string]string
	headers map[string]string
	// system lets a subclass adjust the system blocks (Claude Code).
	system func(blocks []any) []any
	// prepare and recoverAuth are the OAuth hooks, as in the openaichat package.
	recoverAuth func(ctx context.Context, err error) bool
	// wrapErr rewrites failures before they reach the caller (Claude Code).
	wrapErr func(error) error
}

// New is the Anthropic API, or any endpoint that speaks it (MiniMax) when
// cfg.BaseURL is set. The provider keeps the name "anthropic" in both cases.
func New(cfg transport.Config) *Provider {
	base := cfg.BaseURL
	if base == "" {
		base = defaultBaseURL
		cfg.TrustedBaseURL = cfg.TrustedBaseURL || true
	}
	key := cfg.APIKey
	return &Provider{
		hc: cfg.Client(), baseURL: base, headers: cfg.Headers,
		auth: func() map[string]string { return map[string]string{"x-api-key": key} },
	}
}

func (p *Provider) Name() string { return "anthropic" }

// ContextWindow is 200k for every Claude model.
func (p *Provider) ContextWindow(string) int { return 200000 }

func (p *Provider) SupportsVision() bool { return false }

func (p *Provider) requestHeaders(accept string, body bool) map[string]string {
	h := map[string]string{"anthropic-version": apiVersion, "Accept": accept}
	if body {
		h["Content-Type"] = "application/json"
	}
	for k, v := range p.headers {
		h[k] = v
	}
	for k, v := range p.auth() {
		if v != "" {
			h[k] = v
		}
	}
	return h
}

func (p *Provider) post(ctx context.Context, body *transport.Obj) (*http.Response, error) {
	payload, err := transport.Marshal(body)
	if err != nil {
		return nil, err
	}
	resp, err := transport.Do(ctx, p.hc, "anthropic", "Connection error.", transport.Request{
		Method: http.MethodPost, URL: transport.JoinURL(p.baseURL, "/v1/messages"), Header: p.requestHeaders("application/json", true), Body: payload,
	})
	if err != nil {
		return nil, err
	}
	if err := transport.CheckStatus("anthropic", resp, true); err != nil {
		return nil, err
	}
	return resp, nil
}

// postWithRecovery retries once after the OAuth hook renewed the credentials.
func (p *Provider) postWithRecovery(ctx context.Context, body *transport.Obj) (*http.Response, error) {
	resp, err := p.post(ctx, body)
	if err != nil && p.recoverAuth != nil && !llm.IsAbort(err) && p.recoverAuth(ctx, err) {
		return p.post(ctx, body)
	}
	return resp, err
}

func (p *Provider) buildParams(model string, msgs []llm.Message, tools []llm.Tool, opt llm.Options, stream bool) *transport.Obj {
	system, messages := convertMessages(msgs)
	if p.system != nil {
		system = p.system(system)
	}
	maxTokens := opt.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 16384
	}
	params := transport.NewObj().Set("model", model).Set("max_tokens", maxTokens).Set("messages", messages)
	if stream {
		params.Set("stream", true)
	}
	if len(system) > 0 {
		params.Set("system", system)
	}
	if len(tools) > 0 {
		params.Set("tools", formatTools(tools))
	}
	applyCacheBreakpoints(params)
	return params
}

// Chat runs one non-streamed request.
func (p *Provider) Chat(ctx context.Context, msgs []llm.Message, tools []llm.Tool, opt llm.Options) (*llm.Result, error) {
	res, err := p.chat(ctx, msgs, tools, opt)
	return res, p.fail(err)
}

func (p *Provider) fail(err error) error {
	if err != nil && p.wrapErr != nil {
		return p.wrapErr(err)
	}
	return err
}

func (p *Provider) chat(ctx context.Context, msgs []llm.Message, tools []llm.Tool, opt llm.Options) (*llm.Result, error) {
	model, err := openaichat.RequireModel("anthropic", opt)
	if err != nil {
		return nil, err
	}
	resp, err := p.postWithRecovery(ctx, p.buildParams(model, msgs, tools, opt, false))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, &llm.Error{Provider: "anthropic", Message: "Connection error.", Retryable: true}
	}
	parsed, err := transport.ParseOrdered(raw)
	if err != nil {
		return nil, &llm.Error{Provider: "anthropic", Message: "Unexpected token in JSON response from anthropic"}
	}
	doc, _ := parsed.(*transport.Obj)
	if doc == nil {
		doc = transport.NewObj()
	}
	blocks := doc.List("content")
	var content strings.Builder
	var calls []llm.ToolCall
	for _, b := range blocks {
		block, _ := b.(*transport.Obj)
		if block == nil {
			continue
		}
		switch block.String("type") {
		case "text":
			content.WriteString(block.String("text"))
		case "tool_use":
			input, _ := block.Get("input")
			args, _ := transport.Marshal(input)
			calls = append(calls, llm.ToolCall{ID: block.String("id"), Type: "function", Function: llm.FunctionCall{Name: block.String("name"), Arguments: string(args)}})
		}
	}
	// A truncated or error-shaped response can omit usage entirely; a missing
	// token count must not abort the run.
	var usage map[string]any
	if u := doc.Object("usage"); u != nil {
		usage = toMap(u)
	}
	in := promptTokens(usage)
	finish := "stop"
	if doc.String("stop_reason") == "tool_use" {
		finish = "tool_calls"
	}
	return &llm.Result{
		Content: content.String(), ToolCalls: calls, FinishReason: finish, Model: doc.String("model"),
		ReasoningState: map[string]any{ProviderContentBlocksKey: normalizeContentBlocks(blocks)},
		Usage: &llm.Usage{
			InputTokens: in, OutputTokens: num(usage["output_tokens"]),
			CachedReadTokens: num(usage["cache_read_input_tokens"]), CacheWriteTokens: num(usage["cache_creation_input_tokens"]),
		},
	}, nil
}

type streamBlock struct {
	typ       string
	id, name  string
	text      string
	thinking  string
	signature string
	data      any
	inputJSON string
}

// Stream yields text deltas and a final done event carrying tool calls and the
// provider content blocks.
func (p *Provider) Stream(ctx context.Context, msgs []llm.Message, tools []llm.Tool, opt llm.Options) iter.Seq2[llm.Event, error] {
	return func(yield func(llm.Event, error) bool) {
		inner := yield
		yield = func(ev llm.Event, err error) bool { return inner(ev, p.fail(err)) }
		model, err := openaichat.RequireModel("anthropic", opt)
		if err != nil {
			yield(llm.Event{}, err)
			return
		}
		resp, err := p.postWithRecovery(ctx, p.buildParams(model, msgs, tools, opt, true))
		if err != nil {
			yield(llm.Event{}, err)
			return
		}
		defer resp.Body.Close()

		var (
			content  strings.Builder
			calls    []llm.ToolCall
			blocks   = map[int]*streamBlock{}
			usage    *llm.Usage
			stopped  bool
			consumer = true
		)
		err = transport.ReadSSE(resp.Body, func(ev transport.SSEEvent) error {
			if ev.Event == "ping" || ev.Data == "" {
				return nil
			}
			var e streamEvent
			if err := json.Unmarshal([]byte(ev.Data), &e); err != nil {
				return &llm.Error{Provider: "anthropic", Message: "Could not parse stream event as JSON"}
			}
			if ev.Event == "error" || e.Type == "error" {
				return llm.StatusError("anthropic", 0, nil, []byte(ev.Data), true)
			}
			switch e.Type {
			case "message_start":
				if e.Message.Usage != nil {
					u := e.Message.Usage
					usage = &llm.Usage{InputTokens: promptTokens(u), OutputTokens: num(u["output_tokens"]),
						CachedReadTokens: num(u["cache_read_input_tokens"]), CacheWriteTokens: num(u["cache_creation_input_tokens"])}
				}
			case "content_block_start":
				b := e.ContentBlock
				switch b.Type {
				case "thinking":
					blocks[e.Index] = &streamBlock{typ: "thinking", thinking: b.Thinking, signature: b.Signature}
				case "redacted_thinking":
					blocks[e.Index] = &streamBlock{typ: "redacted_thinking", data: b.Data}
				case "text":
					blocks[e.Index] = &streamBlock{typ: "text", text: b.Text}
				case "tool_use":
					calls = append(calls, llm.ToolCall{ID: b.ID, Type: "function", Function: llm.FunctionCall{Name: b.Name}})
					blocks[e.Index] = &streamBlock{typ: "tool_use", id: b.ID, name: b.Name}
				}
			case "content_block_delta":
				block := blocks[e.Index]
				switch e.Delta.Type {
				case "text_delta":
					content.WriteString(e.Delta.Text)
					if block != nil && block.typ == "text" {
						block.text += e.Delta.Text
					}
					if !yield(llm.Event{Type: llm.EventContent, Content: e.Delta.Text}, nil) {
						consumer = false
						return io.EOF
					}
				case "thinking_delta":
					if block != nil && block.typ == "thinking" {
						block.thinking += e.Delta.Thinking
					}
				case "signature_delta":
					if block != nil && block.typ == "thinking" {
						block.signature = e.Delta.Signature
					}
				case "input_json_delta":
					if len(calls) > 0 {
						calls[len(calls)-1].Function.Arguments += e.Delta.PartialJSON
					}
					if block != nil && block.typ == "tool_use" {
						block.inputJSON += e.Delta.PartialJSON
					}
				}
			case "message_delta":
				if e.Usage != nil {
					var in, cacheRead, cacheWrite int
					if usage != nil {
						in, cacheRead, cacheWrite = usage.InputTokens, usage.CachedReadTokens, usage.CacheWriteTokens
					}
					usage = &llm.Usage{InputTokens: in, OutputTokens: num(e.Usage["output_tokens"]), CachedReadTokens: cacheRead, CacheWriteTokens: cacheWrite}
				}
			case "message_stop":
				stopped = true
				finish := "stop"
				if len(calls) > 0 {
					finish = "tool_calls"
				}
				consumer = yield(llm.Event{
					Type: llm.EventDone, Content: content.String(), ToolCalls: calls, FinishReason: finish, Usage: usage,
					ReasoningState: map[string]any{ProviderContentBlocksKey: finalBlocks(blocks)},
				}, nil)
				return io.EOF
			}
			return nil
		})
		if !consumer {
			return
		}
		if err != nil && err != io.EOF {
			if ctx.Err() != nil {
				yield(llm.Event{}, ctx.Err())
				return
			}
			yield(llm.Event{}, streamFailure(err))
			return
		}
		if !stopped {
			yield(llm.Event{}, &llm.Error{Provider: "anthropic", Message: "terminated", Retryable: true})
		}
	}
}

// finalBlocks lists the streamed blocks in index order as the API takes them
// back, with each tool input parsed from its streamed JSON.
func finalBlocks(blocks map[int]*streamBlock) []any {
	out := []any{}
	for i := 0; i <= maxIndex(blocks); i++ {
		b := blocks[i]
		if b == nil {
			continue
		}
		switch b.typ {
		case "tool_use":
			var input any = transport.NewObj()
			if strings.TrimSpace(b.inputJSON) != "" {
				if parsed, err := transport.ParseOrdered([]byte(b.inputJSON)); err == nil {
					input = parsed
				}
			}
			out = append(out, transport.NewObj().Set("type", "tool_use").Set("id", b.id).Set("name", b.name).Set("input", input))
		case "thinking":
			o := transport.NewObj().Set("type", "thinking").Set("thinking", b.thinking)
			if b.signature != "" {
				o.Set("signature", b.signature)
			}
			out = append(out, o)
		case "redacted_thinking":
			out = append(out, transport.NewObj().Set("type", "redacted_thinking").Set("data", b.data))
		default:
			out = append(out, transport.NewObj().Set("type", "text").Set("text", b.text))
		}
	}
	return out
}

func maxIndex(blocks map[int]*streamBlock) int {
	m := -1
	for i := range blocks {
		if i > m {
			m = i
		}
	}
	return m
}

type streamEvent struct {
	Type    string `json:"type"`
	Index   int    `json:"index"`
	Message struct {
		Usage map[string]any `json:"usage"`
	} `json:"message"`
	ContentBlock struct {
		Type      string `json:"type"`
		ID        string `json:"id"`
		Name      string `json:"name"`
		Text      string `json:"text"`
		Thinking  string `json:"thinking"`
		Signature string `json:"signature"`
		Data      any    `json:"data"`
	} `json:"content_block"`
	Delta struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		Thinking    string `json:"thinking"`
		Signature   string `json:"signature"`
		PartialJSON string `json:"partial_json"`
	} `json:"delta"`
	Usage map[string]any `json:"usage"`
}

func streamFailure(err error) error {
	if llm.AsError(err) != nil {
		return err
	}
	return &llm.Error{Provider: "anthropic", Message: "terminated", Retryable: true}
}

// Models lists the live catalog.
func (p *Provider) Models(ctx context.Context) ([]llm.Model, error) {
	entries, err := p.Catalog(ctx)
	if err != nil {
		return nil, err
	}
	models := make([]llm.Model, len(entries))
	for i, e := range entries {
		models[i] = e.Model
	}
	return models, nil
}

// Catalog lists the live catalog (first 100 models) with creation times.
func (p *Provider) Catalog(ctx context.Context) ([]llm.CatalogModel, error) {
	models, err := p.catalog(ctx)
	return models, p.fail(err)
}

func (p *Provider) catalog(ctx context.Context) ([]llm.CatalogModel, error) {
	listOnce := func() (map[string]any, error) {
		resp, err := transport.Do(ctx, p.hc, "anthropic", "Connection error.", transport.Request{
			URL: transport.JoinURL(p.baseURL, "/v1/models?limit=100"), Header: p.requestHeaders("application/json", false),
		})
		if err != nil {
			return nil, err
		}
		if err := transport.CheckStatus("anthropic", resp, true); err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		raw, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, &llm.Error{Provider: "anthropic", Message: "Connection error.", Retryable: true}
		}
		var doc map[string]any
		if json.Unmarshal(raw, &doc) != nil {
			return nil, &llm.Error{Provider: "anthropic", Message: "Unexpected token in JSON response from anthropic"}
		}
		return doc, nil
	}
	doc, err := listOnce()
	if err != nil && p.recoverAuth != nil && !llm.IsAbort(err) && p.recoverAuth(ctx, err) {
		doc, err = listOnce()
	}
	if err != nil {
		return nil, err
	}
	data, _ := doc["data"].([]any)
	var out []llm.CatalogModel
	for _, item := range data {
		m, _ := item.(map[string]any)
		id, _ := m["id"].(string)
		name, _ := m["display_name"].(string)
		if name == "" {
			name = id
		}
		createdAt, _ := m["created_at"].(string)
		out = append(out, llm.CatalogModel{Model: llm.Model{ID: id, Name: name, ContextWindow: 200000}, CreatedAt: createdAt, Raw: m})
	}
	return out, nil
}
