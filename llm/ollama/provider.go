// Package ollama serves local models through an Ollama server (/api/chat with
// NDJSON streaming, /api/tags catalog, /api/pull) and its SystemOne decision
// models.
package ollama

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/NeoLabs-Systems/neolabs-go/llm"
	"github.com/NeoLabs-Systems/neolabs-go/llm/openaichat"
	"github.com/NeoLabs-Systems/neolabs-go/llm/systemone"
	"github.com/NeoLabs-Systems/neolabs-go/llm/transport"
)

const (
	defaultBaseURL = "http://localhost:11434"
	maxChatBytes   = 16 << 20
	// Ollama 0.35+ tags SystemOne decision models with this capability and
	// serves them through /v1/systemone instead of chat.
	decisionCapability = "decision"
	// CodeToolsUnsupported tags a model that rejects tools; the caller retries without them.
	CodeToolsUnsupported = "OLLAMA_TOOLS_UNSUPPORTED"
)

// Status is a model download progress report.
type Status struct {
	Kind    string // model_download
	Status  string // started, completed, failed
	Model   string
	Phase   string
	Message string
}

// Config configures an Ollama client.
type Config struct {
	transport.Config
	// OnStatus is told about model downloads (optional).
	OnStatus func(Status)
}

// Provider is an Ollama server.
type Provider struct {
	hc       *http.Client
	baseURL  string
	onStatus func(Status)
}

// New returns a client for the Ollama server at cfg.BaseURL (default
// http://localhost:11434). The default and an operator-set address may be
// private; mark a user-supplied one untrusted by leaving TrustedBaseURL false and
// setting BaseURL, as the server then refuses internal addresses.
func New(cfg Config) *Provider {
	base := cfg.BaseURL
	if base == "" {
		base = defaultBaseURL
		cfg.TrustedBaseURL = true
	}
	return &Provider{hc: cfg.Client(), baseURL: base, onStatus: cfg.OnStatus}
}

func (p *Provider) Name() string { return "ollama" }

func (p *Provider) ContextWindow(string) int { return 128000 }

func (p *Provider) SupportsVision() bool { return false }

func ollamaError(message string, status int) *llm.Error {
	e := &llm.Error{Provider: "ollama", Status: status}
	if status == 0 {
		e.Message = "Ollama request failed: " + message
	} else {
		e.Message = fmt.Sprintf("Ollama request failed (HTTP %d): %s", status, message)
		e.Retryable = llm.RetryableStatus(status)
	}
	if toolsUnsupported.MatchString(message) {
		e.Code = CodeToolsUnsupported
	}
	return e
}

var toolsUnsupported = regexp.MustCompile(`(?i)does not support tools|tools.*not supported`)

type tag struct {
	Name         string   `json:"name"`
	Capabilities []string `json:"capabilities"`
}

// listTags reads /api/tags. Failures other than cancellation read as "no
// models": an unreachable server is not an error for the catalog.
func (p *Provider) listTags(ctx context.Context) ([]tag, error) {
	res, err := transport.FetchText(ctx, p.hc, transport.FetchOptions{
		URL: p.baseURL + "/api/tags", MaxBytes: 2 << 20, Service: "Ollama model catalog", Timeout: 5 * time.Second, Provider: "ollama",
	})
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, nil
	}
	if !res.OK() {
		return nil, nil
	}
	var payload struct {
		Models []tag `json:"models"`
	}
	text := res.Text
	if text == "" {
		text = "{}"
	}
	if json.Unmarshal([]byte(text), &payload) != nil {
		return nil, nil
	}
	return payload.Models, nil
}

func hasCapability(t tag, capability string) bool {
	for _, c := range t.Capabilities {
		if c == capability {
			return true
		}
	}
	return false
}

// Models lists chat models: decision models cannot hold a conversation.
func (p *Provider) Models(ctx context.Context) ([]llm.Model, error) {
	tags, err := p.listTags(ctx)
	if err != nil {
		return nil, err
	}
	var out []llm.Model
	for _, t := range tags {
		if !hasCapability(t, decisionCapability) {
			out = append(out, llm.Model{ID: t.Name, Name: t.Name, ContextWindow: 128000})
		}
	}
	return out, nil
}

// EmbeddingModels returns Ollama models whose /api/tags capabilities explicitly
// include embedding. Model names are not examined.
func (p *Provider) EmbeddingModels(ctx context.Context) ([]llm.CatalogModel, error) {
	tags, err := p.listTags(ctx)
	if err != nil {
		return nil, err
	}
	var out []llm.CatalogModel
	for _, item := range tags {
		if hasCapability(item, string(llm.KindEmbedding)) {
			out = append(out, llm.CatalogModel{Model: llm.Model{ID: item.Name, Name: item.Name}, Kind: llm.KindEmbedding})
		}
	}
	return out, nil
}

// Embed calls Ollama's native /api/embed endpoint.
func (p *Provider) Embed(ctx context.Context, texts []string, opt llm.Options) (*llm.EmbeddingResult, error) {
	model, err := openaichat.RequireModel(p.Name(), opt)
	if err != nil {
		return nil, err
	}
	if len(texts) == 0 {
		return nil, fmt.Errorf("ollama embedding input is empty")
	}
	body, err := transport.Marshal(transport.NewObj().Set("model", model).Set("input", texts))
	if err != nil {
		return nil, err
	}
	response, err := transport.FetchText(ctx, p.hc, transport.FetchOptions{
		Method: http.MethodPost, URL: p.baseURL + "/api/embed", Header: map[string]string{"Content-Type": "application/json"}, Body: body,
		MaxBytes: 8 << 20, Service: "Ollama embedding", Provider: p.Name(), Timeout: 30 * time.Second,
	})
	if err != nil {
		return nil, err
	}
	if !response.OK() {
		var payload struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal([]byte(response.Text), &payload)
		return nil, ollamaError(payload.Error, response.Status)
	}
	var payload struct {
		Model           string      `json:"model"`
		Embeddings      [][]float32 `json:"embeddings"`
		PromptEvalCount int         `json:"prompt_eval_count"`
		Error           string      `json:"error"`
	}
	if err := json.Unmarshal([]byte(response.Text), &payload); err != nil || payload.Error != "" {
		message := payload.Error
		if message == "" {
			message = "Ollama returned invalid embedding data."
		}
		return nil, ollamaError(message, response.Status)
	}
	if len(payload.Embeddings) != len(texts) || len(payload.Embeddings) == 0 {
		return nil, &llm.Error{Provider: p.Name(), Code: "INVALID_EMBEDDING_RESPONSE", Message: "Ollama returned the wrong number of embeddings."}
	}
	dimensions := len(payload.Embeddings[0])
	if dimensions == 0 {
		return nil, &llm.Error{Provider: p.Name(), Code: "INVALID_EMBEDDING_RESPONSE", Message: "Ollama returned an empty embedding."}
	}
	for _, vector := range payload.Embeddings {
		if len(vector) != dimensions {
			return nil, &llm.Error{Provider: p.Name(), Code: "INVALID_EMBEDDING_RESPONSE", Message: "Ollama returned vectors with inconsistent dimensions."}
		}
	}
	if payload.Model == "" {
		payload.Model = model
	}
	return &llm.EmbeddingResult{Vectors: payload.Embeddings, Model: payload.Model, Usage: &llm.Usage{InputTokens: payload.PromptEvalCount}}, nil
}

// DecisionModels lists the SystemOne models the server serves.
func (p *Provider) DecisionModels(ctx context.Context) ([]llm.Model, error) {
	tags, err := p.listTags(ctx)
	if err != nil {
		return nil, err
	}
	var out []llm.Model
	for _, t := range tags {
		if hasCapability(t, decisionCapability) {
			out = append(out, llm.Model{ID: t.Name, Name: t.Name})
		}
	}
	return out, nil
}

// Decide asks a SystemOne decision model.
func (p *Provider) Decide(ctx context.Context, req llm.DecisionRequest) (map[string]any, error) {
	return systemone.Post(ctx, p.hc, "ollama", p.baseURL+"/v1/systemone", nil, req)
}

func (p *Provider) report(s Status) {
	if p.onStatus != nil {
		p.onStatus(s)
	}
}

// ensureModel pulls the model from the registry when the server lacks it.
func (p *Provider) ensureModel(ctx context.Context, model string) error {
	models, err := p.Models(ctx)
	if err != nil {
		return err
	}
	normalized := model
	if !strings.Contains(model, ":") {
		normalized = model + ":latest"
	}
	for _, m := range models {
		if m.ID == model || m.ID == normalized {
			return nil
		}
	}
	p.report(Status{Kind: "model_download", Status: "started", Model: model, Phase: "Downloading model",
		Message: fmt.Sprintf("Downloading local Ollama model '%s'. First-time pulls can take a while.", model)})
	body, _ := transport.Marshal(transport.NewObj().Set("name", model).Set("stream", false))
	res, err := transport.FetchText(ctx, p.hc, transport.FetchOptions{
		Method: http.MethodPost, URL: p.baseURL + "/api/pull", Header: map[string]string{"Content-Type": "application/json"}, Body: body,
		MaxBytes: 2 << 20, Service: "Ollama model pull", Timeout: 5 * time.Minute, Provider: "ollama",
	})
	if err == nil && !res.OK() {
		detail := res.Text
		var parsed struct {
			Error string `json:"error"`
		}
		if json.Unmarshal([]byte(res.Text), &parsed) == nil && parsed.Error != "" {
			detail = parsed.Error
		}
		if detail == "" {
			detail = http.StatusText(res.Status)
		}
		if len(detail) > 500 {
			detail = detail[:500]
		}
		err = &llm.Error{Provider: "ollama", Status: res.Status, Retryable: llm.RetryableStatus(res.Status),
			Message: fmt.Sprintf("Pull failed with HTTP %d: %s", res.Status, detail)}
	}
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		p.report(Status{Kind: "model_download", Status: "failed", Model: model, Phase: "Model download failed",
			Message: fmt.Sprintf("Failed to download local Ollama model '%s': %s", model, err.Error())})
		return err
	}
	p.report(Status{Kind: "model_download", Status: "completed", Model: model, Phase: "Thinking",
		Message: fmt.Sprintf("Local Ollama model '%s' is ready.", model)})
	_, err = p.Models(ctx)
	return err
}

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
		if t.Parameters == nil {
			fn.Set("parameters", transport.NewObj().Set("type", "object").Set("properties", transport.NewObj()))
		} else {
			fn.Set("parameters", t.Parameters)
		}
		out = append(out, transport.NewObj().Set("type", "function").Set("function", fn))
	}
	return out
}

func wireMessages(msgs []llm.Message) []any {
	out := make([]any, len(msgs))
	for i, m := range msgs {
		var content any = ""
		if m.Content != nil && m.Content != "" {
			content = m.Content
		}
		o := transport.NewObj().Set("role", m.Role).Set("content", content)
		if m.ToolCallID != "" {
			o.Set("tool_call_id", m.ToolCallID)
		}
		if m.ToolCalls != nil {
			calls := []any{}
			for _, tc := range m.ToolCalls {
				if tc.Function.Name == "" {
					continue
				}
				args := parseArguments(tc.Function.Arguments)
				typ := tc.Type
				call := transport.NewObj().Set("id", tc.ID)
				if typ != "" {
					call.Set("type", typ)
				}
				call.Set("function", transport.NewObj().Set("name", tc.Function.Name).Set("arguments", args))
				calls = append(calls, call)
			}
			o.Set("tool_calls", calls)
		}
		out[i] = o
	}
	return out
}

// parseArguments reads tool arguments as an object, keeping key order.
func parseArguments(raw string) any {
	if strings.TrimSpace(raw) == "" {
		raw = "{}"
	}
	v, err := transport.ParseOrdered([]byte(raw))
	if err != nil {
		return transport.NewObj()
	}
	switch v.(type) {
	case *transport.Obj, []any:
		return v
	}
	return transport.NewObj()
}

func (p *Provider) chatBody(model string, msgs []llm.Message, tools []llm.Tool, opt llm.Options, stream bool) *transport.Obj {
	temperature := 0.7
	if opt.Temperature != nil {
		temperature = *opt.Temperature
	}
	numPredict := opt.MaxTokens
	if numPredict <= 0 {
		numPredict = 16384
	}
	body := transport.NewObj().
		Set("model", model).
		Set("messages", wireMessages(msgs)).
		Set("stream", stream).
		Set("options", transport.NewObj().Set("temperature", temperature).Set("num_predict", numPredict))
	if len(tools) > 0 {
		body.Set("tools", formatTools(tools))
	}
	return body
}

// postChat sends /api/chat. Ollama answers HTTP 200 with an error body for some
// failures and a non-2xx status for others; both become real errors. Models that
// reject tools are tagged so the caller can retry without them.
func (p *Provider) postChat(ctx context.Context, body *transport.Obj) (*http.Response, error) {
	payload, err := transport.Marshal(body)
	if err != nil {
		return nil, err
	}
	resp, err := transport.Do(ctx, p.hc, "ollama", "fetch failed", transport.Request{
		Method: http.MethodPost, URL: p.baseURL + "/api/chat", Header: map[string]string{"Content-Type": "application/json"}, Body: payload,
	})
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		message := string(detail)
		var parsed struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(detail, &parsed) == nil && parsed.Error != "" {
			message = parsed.Error
		}
		if message == "" {
			message = http.StatusText(resp.StatusCode)
		}
		e := ollamaError(message, resp.StatusCode)
		e.RetryAfter = llm.RetryAfterFrom(resp.Header, time.Now())
		return nil, e
	}
	return resp, nil
}

type chatMessage struct {
	Content   string `json:"content"`
	ToolCalls []struct {
		Function struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		} `json:"function"`
	} `json:"tool_calls"`
}

type chatResponse struct {
	Model           string          `json:"model"`
	Message         chatMessage     `json:"message"`
	Done            bool            `json:"done"`
	Error           json.RawMessage `json:"error"`
	PromptEvalCount *float64        `json:"prompt_eval_count"`
	EvalCount       *float64        `json:"eval_count"`
}

func (r chatResponse) errorText() string {
	if len(r.Error) == 0 || string(r.Error) == "null" || string(r.Error) == `""` {
		return ""
	}
	var s string
	if json.Unmarshal(r.Error, &s) == nil {
		return s
	}
	return string(r.Error)
}

// callArguments is the JSON text of a tool call's arguments object.
func callArguments(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return "{}"
	}
	var asString string
	if json.Unmarshal(raw, &asString) == nil {
		return asString
	}
	v, err := transport.ParseOrdered(raw)
	if err != nil {
		return "{}"
	}
	out, _ := transport.Marshal(v)
	return string(out)
}

func callID(seed int64, i int) string { return fmt.Sprintf("call_ollama_%d_%d", seed, i) }

// Chat runs one non-streamed request.
func (p *Provider) Chat(ctx context.Context, msgs []llm.Message, tools []llm.Tool, opt llm.Options) (*llm.Result, error) {
	model, err := openaichat.RequireModel("ollama", opt)
	if err != nil {
		return nil, err
	}
	if err := p.ensureModel(ctx, model); err != nil {
		return nil, err
	}
	data, err := p.readChat(ctx, p.chatBody(model, msgs, tools, opt, false))
	if err != nil && llm.AsError(err) != nil && llm.AsError(err).Code == CodeToolsUnsupported && len(tools) > 0 {
		data, err = p.readChat(ctx, p.chatBody(model, msgs, nil, opt, false))
	}
	if err != nil {
		return nil, err
	}
	seed := time.Now().UnixMilli()
	var calls []llm.ToolCall
	for i, tc := range data.Message.ToolCalls {
		calls = append(calls, llm.ToolCall{ID: callID(seed, i), Type: "function", Function: llm.FunctionCall{Name: tc.Function.Name, Arguments: callArguments(tc.Function.Arguments)}})
	}
	result := &llm.Result{Content: data.Message.Content, ToolCalls: calls, FinishReason: "stop", Model: data.Model}
	if result.Model == "" {
		result.Model = model
	}
	if len(calls) > 0 {
		result.FinishReason = "tool_calls"
	}
	if data.PromptEvalCount != nil && *data.PromptEvalCount != 0 {
		result.Usage = &llm.Usage{InputTokens: int(*data.PromptEvalCount)}
		if data.EvalCount != nil {
			result.Usage.OutputTokens = int(*data.EvalCount)
		}
	}
	return result, nil
}

func (p *Provider) readChat(ctx context.Context, body *transport.Obj) (*chatResponse, error) {
	resp, err := p.postChat(ctx, body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := transport.ReadBody(resp.Body, maxChatBytes, "Ollama chat")
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, err
	}
	var data chatResponse
	if json.Unmarshal(raw, &data) != nil {
		return nil, &llm.Error{Provider: "ollama", Message: "Ollama /api/chat returned malformed JSON."}
	}
	if msg := data.errorText(); msg != "" {
		return nil, ollamaError(msg, 0)
	}
	return &data, nil
}

// Stream yields content deltas and a final done event.
func (p *Provider) Stream(ctx context.Context, msgs []llm.Message, tools []llm.Tool, opt llm.Options) iter.Seq2[llm.Event, error] {
	return func(yield func(llm.Event, error) bool) {
		model, err := openaichat.RequireModel("ollama", opt)
		if err != nil {
			yield(llm.Event{}, err)
			return
		}
		if err := p.ensureModel(ctx, model); err != nil {
			yield(llm.Event{}, err)
			return
		}
		requestTools := tools
		retried := false
		for {
			emitted := false
			err := p.streamOnce(ctx, p.chatBody(model, msgs, requestTools, opt, true), func(ev llm.Event) bool {
				emitted = true
				return yield(ev, nil)
			})
			if err == nil || err == errConsumerStopped {
				return
			}
			if e := llm.AsError(err); e != nil && e.Code == CodeToolsUnsupported && len(tools) > 0 && !retried && !emitted {
				retried = true
				requestTools = nil
				continue
			}
			yield(llm.Event{}, err)
			return
		}
	}
}

var errConsumerStopped = fmt.Errorf("stream consumer stopped")

func malformedStream() error {
	return &llm.Error{Provider: "ollama", Code: "OLLAMA_STREAM_MALFORMED", Message: "Ollama /api/chat returned malformed streaming JSON."}
}

func (p *Provider) streamOnce(ctx context.Context, body *transport.Obj, emit func(llm.Event) bool) error {
	resp, err := p.postChat(ctx, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var (
		content   strings.Builder
		calls     []llm.ToolCall
		seed      = time.Now().UnixMilli()
		next      int
		total     int64
		done      bool
		finalData chatResponse
	)
	handle := func(line string) error {
		if strings.TrimSpace(line) == "" {
			return nil
		}
		var raw map[string]any
		if json.Unmarshal([]byte(line), &raw) != nil || raw == nil {
			return malformedStream()
		}
		var data chatResponse
		if json.Unmarshal([]byte(line), &data) != nil {
			return malformedStream()
		}
		if msg := data.errorText(); msg != "" {
			return ollamaError(msg, 0)
		}
		if data.Message.Content != "" {
			content.WriteString(data.Message.Content)
			if !emit(llm.Event{Type: llm.EventContent, Content: data.Message.Content}) {
				return errConsumerStopped
			}
		}
		for _, tc := range data.Message.ToolCalls {
			name := strings.TrimSpace(tc.Function.Name)
			if name == "" {
				return malformedStream()
			}
			calls = append(calls, llm.ToolCall{ID: callID(seed, next), Type: "function", Function: llm.FunctionCall{Name: name, Arguments: callArguments(tc.Function.Arguments)}})
			next++
		}
		if data.Done {
			done, finalData = true, data
		}
		return nil
	}

	reader := bufio.NewReaderSize(resp.Body, 64<<10)
	for !done {
		line, err := reader.ReadString('\n')
		total += int64(len(line))
		if total > maxChatBytes {
			return &llm.Error{Provider: "ollama", Code: llm.CodeResponseTooLarge, Message: "Ollama stream exceeded its response safety limit."}
		}
		if len(line) > 0 {
			if herr := handle(strings.TrimSuffix(line, "\n")); herr != nil {
				return herr
			}
		}
		if err != nil {
			if err != io.EOF {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				return &llm.Error{Provider: "ollama", Message: "terminated", Retryable: true}
			}
			break
		}
	}
	if !done {
		return &llm.Error{Provider: "ollama", Code: "OLLAMA_STREAM_INCOMPLETE", Message: "Ollama stream ended before sending a completion marker."}
	}
	ev := llm.Event{Type: llm.EventDone, Content: content.String(), ToolCalls: calls, FinishReason: "stop"}
	if len(calls) > 0 {
		ev.FinishReason = "tool_calls"
	}
	if finalData.PromptEvalCount != nil || finalData.EvalCount != nil {
		u := &llm.Usage{}
		if finalData.PromptEvalCount != nil {
			u.InputTokens = int(*finalData.PromptEvalCount)
		}
		if finalData.EvalCount != nil {
			u.OutputTokens = int(*finalData.EvalCount)
		}
		ev.Usage = u
	}
	if !emit(ev) {
		return errConsumerStopped
	}
	return nil
}
