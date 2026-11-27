// Package openaichat serves every provider that speaks the OpenAI Chat
// Completions API: OpenAI, custom OpenAI-compatible endpoints, xAI Grok (key and
// OAuth), NVIDIA NIM, Nous, OpenRouter, GitHub Copilot and NeoRouter. They share
// one transcript conversion, one stream reader with output guards, and one usage
// normalisation; each differs only in request parameters, catalog and errors.
package openaichat

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"iter"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/NeoLabs-Systems/neolabs-go/llm"
	"github.com/NeoLabs-Systems/neolabs-go/llm/transport"
)

// Provider is one configured OpenAI-compatible backend.
type Provider struct {
	name    string
	hc      *http.Client
	baseURL string
	token   func() string
	headers map[string]string

	vision bool
	window func(model string) int
	// build assembles the request body without the stream flags.
	build          func(model string, msgs []any, tools []llm.Tool, opt llm.Options) *transport.Obj
	messages       func(msgs []llm.Message, model string) []any
	streamUsage    bool   // ask for usage in the final stream chunk
	requestPrefix  string // wraps request errors ("" leaves them as the SDK words them)
	streamPrefix   string
	plainChat      bool // OpenAI: the answer is read without reasoning state or truncation
	checkResponse  func(resp map[string]any) error
	retryChat      func(err error, body *transport.Obj) *transport.Obj
	prepare        func(ctx context.Context) error
	recoverAuth    func(ctx context.Context, err error, attemptedToken string) bool
	catalog        func(ctx context.Context) ([]llm.CatalogModel, error)
	live           func(ctx context.Context) ([]llm.CatalogModel, error)
	decideURL      func() string
	decideHeaders  func() map[string]string
	decisionModels func(ctx context.Context) ([]llm.CatalogModel, error)
	headerTimeout  time.Duration // how long to wait for response headers (0: no limit)
}

// Name is the provider key.
func (p *Provider) Name() string { return p.name }

// ContextWindow is the model's context window in tokens.
func (p *Provider) ContextWindow(model string) int { return p.window(model) }

// SupportsVision reports whether the provider accepts images.
func (p *Provider) SupportsVision() bool { return p.vision }

func (p *Provider) requireModel(opt llm.Options) (string, error) {
	return RequireModel(p.name, opt)
}

// RequireModel is the explicit-model rule every provider shares.
func RequireModel(provider string, opt llm.Options) (string, error) {
	if model := strings.TrimSpace(opt.Model); model != "" {
		return model, nil
	}
	return "", &llm.Error{Provider: provider, Code: llm.CodeModelSelectionRequired, Message: "Provider '" + provider + "' requires a model selected from its live catalog."}
}

func (p *Provider) authHeaders(accept string) map[string]string {
	h := map[string]string{"Accept": accept, "Content-Type": "application/json"}
	for k, v := range p.headers {
		h[k] = v
	}
	if key := p.token(); key != "" {
		h["Authorization"] = "Bearer " + key
	}
	return h
}

// get reads a JSON document from the API.
func (p *Provider) get(ctx context.Context, path string) (map[string]any, error) {
	resp, err := transport.Do(ctx, p.hc, p.name, "Connection error.", transport.Request{URL: transport.JoinURL(p.baseURL, path), Header: p.authHeaders("application/json")})
	if err != nil {
		return nil, err
	}
	return p.readJSON(resp)
}

func (p *Provider) readJSON(resp *http.Response) (map[string]any, error) {
	if err := transport.CheckStatus(p.name, resp, false); err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, &llm.Error{Provider: p.name, Message: "Connection error.", Retryable: true}
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, &llm.Error{Provider: p.name, Message: "Unexpected token in JSON response from " + p.name}
	}
	return out, nil
}

func (p *Provider) post(ctx context.Context, path string, body *transport.Obj) (*http.Response, error) {
	payload, err := transport.Marshal(body)
	if err != nil {
		return nil, err
	}
	req := transport.Request{Method: http.MethodPost, URL: transport.JoinURL(p.baseURL, path), Header: p.authHeaders("application/json"), Body: payload}
	if p.headerTimeout <= 0 {
		return transport.Do(ctx, p.hc, p.name, "Connection error.", req)
	}
	reqCtx, cancel := context.WithCancel(ctx)
	timer := time.AfterFunc(p.headerTimeout, cancel)
	resp, err := transport.Do(reqCtx, p.hc, p.name, "Connection error.", req)
	if !timer.Stop() && ctx.Err() == nil {
		// The deadline for response headers passed.
		cancel()
		if resp != nil {
			resp.Body.Close()
		}
		return nil, &llm.Error{Provider: p.name, Message: "Request timed out.", Retryable: true}
	}
	if err != nil {
		cancel()
		return nil, err
	}
	resp.Body = &cancelOnClose{ReadCloser: resp.Body, cancel: cancel}
	return resp, nil
}

type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c *cancelOnClose) Close() error {
	err := c.ReadCloser.Close()
	c.cancel()
	return err
}

// withAuthRetry runs op after prepare and retries it once when recoverAuth
// manages to renew the credentials.
func withAuthRetry[T any](ctx context.Context, p *Provider, op func() (T, error)) (T, error) {
	var zero T
	if p.prepare != nil {
		if err := p.prepare(ctx); err != nil {
			return zero, err
		}
	}
	attempted := p.token()
	value, err := op()
	if err != nil && p.recoverAuth != nil && !llm.IsAbort(err) && p.recoverAuth(ctx, err, attempted) {
		return op()
	}
	return value, err
}

func (p *Provider) wrap(err error, prefix string) error {
	if prefix == "" || err == nil {
		return err
	}
	return llm.WrapError(p.name, err, prefix)
}

// request is the provider's body for one completion, without the stream flags.
func (p *Provider) request(model string, msgs []llm.Message, tools []llm.Tool, opt llm.Options) *transport.Obj {
	body := p.build(model, p.messages(msgs, model), tools, opt)
	if opt.JSON {
		body.Set("response_format", transport.NewObj().Set("type", "json_object"))
	}
	return body
}

// Chat runs one non-streamed completion.
func (p *Provider) Chat(ctx context.Context, msgs []llm.Message, tools []llm.Tool, opt llm.Options) (*llm.Result, error) {
	model, err := p.requireModel(opt)
	if err != nil {
		return nil, err
	}
	body := p.request(model, msgs, tools, opt)
	request := func(body *transport.Obj) (map[string]any, error) {
		return withAuthRetry(ctx, p, func() (map[string]any, error) {
			r, err := p.post(ctx, "/chat/completions", body)
			if err != nil {
				return nil, err
			}
			return p.readJSON(r)
		})
	}
	resp, err := request(body)
	if err != nil && p.retryChat != nil {
		if retry := p.retryChat(err, body); retry != nil {
			resp, err = request(retry)
		}
	}
	if err != nil {
		return nil, p.wrap(err, p.requestPrefix)
	}
	if p.checkResponse != nil {
		if err := p.checkResponse(resp); err != nil {
			return nil, err
		}
	}
	return p.normalizeResponse(resp)
}

// Stream yields content deltas and one final tool_calls or done event.
func (p *Provider) Stream(ctx context.Context, msgs []llm.Message, tools []llm.Tool, opt llm.Options) iter.Seq2[llm.Event, error] {
	return func(yield func(llm.Event, error) bool) {
		model, err := p.requireModel(opt)
		if err != nil {
			yield(llm.Event{}, err)
			return
		}
		body := p.request(model, msgs, tools, opt)
		body.Set("stream", true)
		if p.streamUsage {
			body.Set("stream_options", transport.NewObj().Set("include_usage", true))
		}
		run := func() error {
			resp, err := withAuthRetry(ctx, p, func() (*http.Response, error) {
				r, err := p.post(ctx, "/chat/completions", body)
				if err != nil {
					return nil, err
				}
				if err := transport.CheckStatus(p.name, r, false); err != nil {
					return nil, err
				}
				return r, nil
			})
			if err != nil {
				return p.wrap(err, p.requestPrefix)
			}
			defer resp.Body.Close()
			err = p.readStream(ctx, resp.Body, tools, func(ev llm.Event) bool { return yield(ev, nil) })
			return p.wrap(err, p.streamPrefix)
		}
		err = run()
		if err != nil && !errors.Is(err, errStopped) {
			yield(llm.Event{}, err)
		}
	}
}

// Models lists the live chat catalog.
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

// Catalog lists the live chat catalog with everything the vendor stated.
func (p *Provider) Catalog(ctx context.Context) ([]llm.CatalogModel, error) {
	return withAuthRetry(ctx, p, func() ([]llm.CatalogModel, error) { return p.catalog(ctx) })
}

// LiveVoiceModels lists conversation models of the live API, where the provider has any.
func (p *Provider) LiveVoiceModels(ctx context.Context) ([]llm.CatalogModel, error) {
	if p.live == nil {
		return nil, nil
	}
	return withAuthRetry(ctx, p, func() ([]llm.CatalogModel, error) { return p.live(ctx) })
}

// listIDs lists /models entries whose id passes keep, as catalog models.
func (p *Provider) listIDs(ctx context.Context, keep func(id string) bool, errPrefix string) ([]llm.CatalogModel, error) {
	doc, err := p.get(ctx, "/models")
	if err != nil {
		return nil, p.wrap(err, errPrefix)
	}
	data, _ := doc["data"].([]any)
	var out []llm.CatalogModel
	for _, item := range data {
		m, _ := item.(map[string]any)
		id, _ := m["id"].(string)
		if id == "" || !keep(id) {
			continue
		}
		created, _ := m["created"].(float64)
		out = append(out, llm.CatalogModel{
			Model: llm.Model{ID: id, Name: id, ContextWindow: p.window(id), Vision: p.vision},
			Kind:  llm.ExplicitModelKind(m), Created: int64(created), Raw: m,
		})
	}
	return out, nil
}

var imagePrompt = "Describe this image in detail."

// AnalyzeImage asks a vision model about an image.
func (p *Provider) AnalyzeImage(ctx context.Context, req llm.ImageRequest) (string, error) {
	if !p.vision {
		return "", errors.New("Provider '" + p.name + "' does not support image analysis")
	}
	model, err := p.requireModel(llm.Options{Model: req.Model})
	if err != nil {
		return "", err
	}
	mime := req.MIMEType
	if mime == "" {
		mime = "image/jpeg"
	}
	prompt := req.Prompt
	if prompt == "" {
		prompt = imagePrompt
	}
	body := transport.NewObj().
		Set("model", model).
		Set("max_tokens", 4096).
		Set("messages", []any{transport.NewObj().
			Set("role", "user").
			Set("content", []any{
				transport.NewObj().Set("type", "text").Set("text", prompt),
				transport.NewObj().Set("type", "image_url").Set("image_url", transport.NewObj().Set("url", "data:"+mime+";base64,"+req.ImageB64)),
			})})
	doc, err := withAuthRetry(ctx, p, func() (map[string]any, error) {
		r, err := p.post(ctx, "/chat/completions", body)
		if err != nil {
			return nil, err
		}
		return p.readJSON(r)
	})
	if err != nil {
		return "", err
	}
	if choices, _ := doc["choices"].([]any); len(choices) > 0 {
		if choice, _ := choices[0].(map[string]any); choice != nil {
			if msg, _ := choice["message"].(map[string]any); msg != nil {
				if text, _ := msg["content"].(string); text != "" {
					return text, nil
				}
			}
		}
	}
	return "", nil
}

var reasoningModelID = regexp.MustCompile(`^o\d|^gpt-(?:[5-9]|\d{2,})(?:[.\-]|$)|reasoning`)

// IsReasoningModel is OpenAI's rule for models that think by default.
func IsReasoningModel(model string) bool {
	return reasoningModelID.MatchString(strings.ToLower(strings.TrimSpace(model)))
}
