package google

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"math/big"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/NeoLabs-Systems/neolabs-go/llm"
	"github.com/NeoLabs-Systems/neolabs-go/llm/openaichat"
	"github.com/NeoLabs-Systems/neolabs-go/llm/transport"
)

const defaultBaseURL = "https://generativelanguage.googleapis.com"

// Cache holds what discovery learned about models: which ones think and how
// large their context is. Runs build fresh providers after discovery, so share
// one Cache per process. A provider given nil gets a private one.
type Cache struct {
	mu       sync.Mutex
	thinking map[string]bool
	windows  map[string]int
}

// NewCache returns an empty cache.
func NewCache() *Cache {
	return &Cache{thinking: map[string]bool{}, windows: map[string]int{}}
}

func (c *Cache) isThinking(model string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.thinking[model]
}

// Provider is the Gemini client.
type Provider struct {
	hc      *http.Client
	baseURL string
	apiKey  string
	cache   *Cache
}

// New returns a Gemini client; cfg.APIKey is the Google AI key.
func New(cfg transport.Config, cache *Cache) *Provider {
	if cache == nil {
		cache = NewCache()
	}
	base := cfg.BaseURL
	if base == "" {
		base = defaultBaseURL
		cfg.TrustedBaseURL = true
	}
	return &Provider{hc: cfg.Client(), baseURL: strings.TrimRight(base, "/"), apiKey: cfg.APIKey, cache: cache}
}

func (p *Provider) Name() string { return "google" }

func (p *Provider) SupportsVision() bool { return false }

// ContextWindow is the catalog's input token limit, 1M tokens when unknown.
func (p *Provider) ContextWindow(model string) int {
	p.cache.mu.Lock()
	defer p.cache.mu.Unlock()
	if w := p.cache.windows[model]; w > 0 {
		return w
	}
	return 1048576
}

func modelPath(model string) string {
	if strings.Contains(model, "/") {
		return model
	}
	return "models/" + model
}

func (p *Provider) post(ctx context.Context, model, method string, body *transport.Obj) (*http.Response, error) {
	payload, err := transport.Marshal(body)
	if err != nil {
		return nil, err
	}
	target := p.baseURL + "/v1beta/" + modelPath(model) + ":" + method
	if method == "streamGenerateContent" {
		target += "?alt=sse"
	}
	resp, err := transport.Do(ctx, p.hc, "google", "fetch failed", transport.Request{
		Method: http.MethodPost, URL: target, Body: payload,
		Header: map[string]string{"x-goog-api-key": p.apiKey, "Content-Type": "application/json"},
	})
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 && resp.StatusCode < 600 {
		return nil, p.statusError(resp)
	}
	if err := transport.CheckStatus("google", resp, true); err != nil {
		return nil, err
	}
	return resp, nil
}

// statusError words a failure like the Google SDK: the message is the JSON of
// the error body.
func (p *Provider) statusError(resp *http.Response) error {
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	var parsed any
	if strings.Contains(resp.Header.Get("Content-Type"), "json") && json.Unmarshal(body, &parsed) == nil {
		body, _ = transport.Marshal(parsed)
	} else {
		body, _ = transport.Marshal(map[string]any{"error": map[string]any{"message": string(body), "code": resp.StatusCode, "status": resp.Status}})
	}
	return &llm.Error{Provider: "google", Status: resp.StatusCode, Message: string(body), Retryable: llm.RetryableStatus(resp.StatusCode), RetryAfter: llm.RetryAfterFrom(resp.Header, time.Now())}
}

func generateID() string {
	n, _ := rand.Int(rand.Reader, big.NewInt(36*36*36*36*36*36))
	return fmt.Sprintf("call_%d_%s", time.Now().UnixMilli(), fmt.Sprintf("%06s", n.Text(36)))
}

// collected is what the parts of one response contribute.
type collected struct {
	content    string
	calls      []llm.ToolCall
	signatures map[string]any
}

// collect adds the parts of a response: text (thoughts excluded) and function
// calls. seen drops a call the stream repeats.
func (c *collected) collect(doc *transport.Obj, seen map[string]bool) string {
	var content strings.Builder
	for _, cand := range doc.List("candidates") {
		candidate, _ := cand.(*transport.Obj)
		if candidate == nil || candidate.Object("content") == nil {
			continue
		}
		for _, pt := range candidate.Object("content").List("parts") {
			part, _ := pt.(*transport.Obj)
			if part == nil {
				continue
			}
			if text := part.String("text"); text != "" {
				if thought, _ := part.Get("thought"); thought != true {
					content.WriteString(text)
				}
			}
			fc := part.Object("functionCall")
			if fc == nil || fc.String("name") == "" {
				continue
			}
			args := any(transport.NewObj())
			if a, ok := fc.Get("args"); ok && a != nil {
				args = a
			}
			encoded, _ := transport.Marshal(args)
			providerID := strings.TrimSpace(fc.String("id"))
			signature := providerID
			if signature == "" {
				signature = fc.String("name") + ":" + string(encoded)
			}
			if seen != nil {
				if seen[signature] {
					continue
				}
				seen[signature] = true
			}
			id := providerID
			if id == "" {
				id = generateID()
			}
			c.calls = append(c.calls, llm.ToolCall{ID: id, Type: "function", Function: llm.FunctionCall{Name: fc.String("name"), Arguments: string(encoded)}})
			if sig := part.String("thoughtSignature"); sig != "" {
				if c.signatures == nil {
					c.signatures = map[string]any{}
				}
				c.signatures[id] = sig
			}
		}
	}
	text := content.String()
	c.content += text
	return text
}

func (c *collected) reasoningState() map[string]any {
	if len(c.signatures) == 0 {
		return nil
	}
	return map[string]any{ThoughtSignaturesKey: c.signatures}
}

func finishReason(calls []llm.ToolCall) string {
	if len(calls) > 0 {
		return "tool_calls"
	}
	return "stop"
}

func usageFrom(u *transport.Obj) *llm.Usage {
	if u == nil {
		return nil
	}
	n := func(key string) int {
		v, _ := u.Get(key)
		f, _ := v.(float64)
		return int(f)
	}
	return &llm.Usage{InputTokens: n("promptTokenCount"), OutputTokens: n("candidatesTokenCount"), ReasoningTokens: n("thoughtsTokenCount"), CachedReadTokens: n("cachedContentTokenCount")}
}

func parseDoc(raw []byte) (*transport.Obj, error) {
	v, err := transport.ParseOrdered(raw)
	doc, _ := v.(*transport.Obj)
	if err != nil || doc == nil {
		return nil, &llm.Error{Provider: "google", Message: "Unexpected token in JSON response from google"}
	}
	return doc, nil
}

// Chat runs one non-streamed generateContent call.
func (p *Provider) Chat(ctx context.Context, msgs []llm.Message, tools []llm.Tool, opt llm.Options) (*llm.Result, error) {
	model, err := openaichat.RequireModel("google", opt)
	if err != nil {
		return nil, err
	}
	resp, err := p.post(ctx, model, "generateContent", p.requestBody(model, msgs, tools, opt))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, &llm.Error{Provider: "google", Message: "fetch failed", Retryable: true}
	}
	doc, err := parseDoc(raw)
	if err != nil {
		return nil, err
	}
	var c collected
	c.collect(doc, nil)
	return &llm.Result{
		Content: c.content, ToolCalls: c.calls, FinishReason: finishReason(c.calls),
		Usage: usageFrom(doc.Object("usageMetadata")), Model: model, ReasoningState: c.reasoningState(),
	}, nil
}

// Stream yields text deltas and a final done event.
func (p *Provider) Stream(ctx context.Context, msgs []llm.Message, tools []llm.Tool, opt llm.Options) iter.Seq2[llm.Event, error] {
	return func(yield func(llm.Event, error) bool) {
		model, err := openaichat.RequireModel("google", opt)
		if err != nil {
			yield(llm.Event{}, err)
			return
		}
		resp, err := p.post(ctx, model, "streamGenerateContent", p.requestBody(model, msgs, tools, opt))
		if err != nil {
			yield(llm.Event{}, err)
			return
		}
		defer resp.Body.Close()
		var c collected
		seen := map[string]bool{}
		var usage *transport.Obj
		stopped := false
		err = transport.ReadSSE(resp.Body, func(ev transport.SSEEvent) error {
			if ev.Data == "" {
				return nil
			}
			doc, err := parseDoc([]byte(ev.Data))
			if err != nil {
				return err
			}
			if text := c.collect(doc, seen); text != "" {
				if !yield(llm.Event{Type: llm.EventContent, Content: text}, nil) {
					stopped = true
					return io.EOF
				}
			}
			if u := doc.Object("usageMetadata"); u != nil {
				usage = u
			}
			return nil
		})
		if stopped {
			return
		}
		if err != nil && err != io.EOF {
			if ctx.Err() != nil {
				yield(llm.Event{}, ctx.Err())
			} else if llm.AsError(err) != nil {
				yield(llm.Event{}, err)
			} else {
				yield(llm.Event{}, &llm.Error{Provider: "google", Message: "terminated", Retryable: true})
			}
			return
		}
		yield(llm.Event{
			Type: llm.EventDone, Content: c.content, ToolCalls: c.calls, FinishReason: finishReason(c.calls),
			Usage: usageFrom(usage), Model: model, ReasoningState: c.reasoningState(),
		}, nil)
	}
}

var googleDrop = regexp.MustCompile(`(?i)tts|lyria|robotics|deep-research|antigravity|computer-use`)

// droppedImageModel is /-image(?!.*it)/i: an "-image" model without an "it"
// (instruction-tuned) suffix after it.
func droppedImageModel(id string) bool {
	lower := strings.ToLower(id)
	at := strings.LastIndex(lower, "-image")
	return at >= 0 && !strings.Contains(lower[at+len("-image"):], "it")
}

func (p *Provider) fetchCatalog(ctx context.Context) ([]map[string]any, error) {
	var models []map[string]any
	seenTokens := map[string]bool{}
	pageToken := ""
	for {
		q := url.Values{"pageSize": {"1000"}}
		if pageToken != "" {
			q.Set("pageToken", pageToken)
		}
		res, err := transport.FetchText(ctx, p.hc, transport.FetchOptions{
			URL: p.baseURL + "/v1beta/models?" + q.Encode(), Header: map[string]string{"x-goog-api-key": p.apiKey},
			MaxBytes: 5 << 20, Service: "Google model catalog", Provider: "google",
		})
		if err != nil {
			return nil, err
		}
		if !res.OK() {
			return nil, &llm.Error{Provider: "google", Status: res.Status, Retryable: llm.RetryableStatus(res.Status),
				RetryAfter: llm.RetryAfterFrom(res.Header, time.Now()), Message: fmt.Sprintf("Google models API returned %d", res.Status)}
		}
		text := res.Text
		if text == "" {
			text = "{}"
		}
		var payload struct {
			Models        []map[string]any `json:"models"`
			NextPageToken string           `json:"nextPageToken"`
		}
		if json.Unmarshal([]byte(text), &payload) != nil {
			return nil, &llm.Error{Provider: "google", Message: "Google models API returned invalid JSON."}
		}
		models = append(models, payload.Models...)
		next := strings.TrimSpace(payload.NextPageToken)
		if next == "" || seenTokens[next] {
			return models, nil
		}
		seenTokens[next] = true
		pageToken = next
	}
}

func supports(model map[string]any, method string) bool {
	list, _ := model["supportedGenerationMethods"].([]any)
	for _, m := range list {
		if m == method {
			return true
		}
	}
	return false
}

func shortName(model map[string]any) string {
	name, _ := model["name"].(string)
	return strings.Replace(name, "models/", "", 1)
}

func displayName(model map[string]any, fallback string) string {
	if d, _ := model["displayName"].(string); d != "" {
		return d
	}
	return fallback
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

// Catalog lists models that can generate content; it also records context
// windows and which models think for later runs.
func (p *Provider) Catalog(ctx context.Context) ([]llm.CatalogModel, error) {
	raw, err := p.fetchCatalog(ctx)
	if err != nil {
		return nil, err
	}
	var out []llm.CatalogModel
	for _, m := range raw {
		id := shortName(m)
		if !supports(m, "generateContent") || googleDrop.MatchString(id) || droppedImageModel(id) {
			continue
		}
		window := 1048576
		if limit, ok := m["inputTokenLimit"].(float64); ok && limit > 0 {
			window = int(limit)
		}
		thinking, _ := m["thinking"].(bool)
		p.cache.mu.Lock()
		p.cache.windows[id] = window
		if thinking {
			p.cache.thinking[id] = true
		}
		p.cache.mu.Unlock()
		out = append(out, llm.CatalogModel{Model: llm.Model{ID: id, Name: displayName(m, id), ContextWindow: window}, Thinking: thinking, Raw: m})
	}
	return out, nil
}

// EmbeddingModels lists only Google models whose catalog explicitly advertises
// embedContent. Chat discovery remains a separate Catalog operation.
func (p *Provider) EmbeddingModels(ctx context.Context) ([]llm.CatalogModel, error) {
	raw, err := p.fetchCatalog(ctx)
	if err != nil {
		return nil, err
	}
	var out []llm.CatalogModel
	for _, model := range raw {
		id := shortName(model)
		if id == "" || !supports(model, "embedContent") {
			continue
		}
		out = append(out, llm.CatalogModel{Model: llm.Model{ID: id, Name: displayName(model, id)}, Kind: llm.KindEmbedding, Raw: model})
	}
	return out, nil
}

// Embed calls Google's batchEmbedContents endpoint.
func (p *Provider) Embed(ctx context.Context, texts []string, opt llm.Options) (*llm.EmbeddingResult, error) {
	model := strings.TrimSpace(opt.Model)
	if model == "" {
		return nil, &llm.Error{Provider: p.Name(), Code: llm.CodeModelSelectionRequired, Message: "An embedding model must be selected."}
	}
	if len(texts) == 0 || len(texts) > 100 {
		return nil, &llm.Error{Provider: p.Name(), Code: "INVALID_EMBEDDING_INPUT", Message: "Google embedding input must contain between 1 and 100 texts."}
	}
	requests := make([]any, 0, len(texts))
	for _, text := range texts {
		requests = append(requests, map[string]any{"model": modelPath(model), "content": map[string]any{"parts": []any{map[string]string{"text": text}}}})
	}
	body, err := transport.Marshal(transport.NewObj().Set("requests", requests))
	if err != nil {
		return nil, err
	}
	endpoint := p.baseURL + "/v1beta/models/" + url.PathEscape(model) + ":batchEmbedContents"
	response, err := transport.FetchText(ctx, p.hc, transport.FetchOptions{
		Method: http.MethodPost, URL: endpoint, Header: map[string]string{"x-goog-api-key": p.apiKey, "Content-Type": "application/json"}, Body: body,
		MaxBytes: 8 << 20, Service: "Google embedding", Provider: p.Name(), Timeout: 30 * time.Second,
	})
	if err != nil {
		return nil, err
	}
	if !response.OK() {
		return nil, &llm.Error{Provider: p.Name(), Status: response.Status, Retryable: llm.RetryableStatus(response.Status), RetryAfter: llm.RetryAfterFrom(response.Header, time.Now()), Message: fmt.Sprintf("Google embedding request failed (HTTP %d).", response.Status)}
	}
	var payload struct {
		Embeddings []struct {
			Values []float32 `json:"values"`
		} `json:"embeddings"`
	}
	if err = json.Unmarshal([]byte(response.Text), &payload); err != nil || len(payload.Embeddings) != len(texts) {
		return nil, &llm.Error{Provider: p.Name(), Code: "INVALID_EMBEDDING_RESPONSE", Message: "Google returned invalid embedding data."}
	}
	vectors := make([][]float32, len(payload.Embeddings))
	dimensions := len(payload.Embeddings[0].Values)
	if dimensions == 0 {
		return nil, &llm.Error{Provider: p.Name(), Code: "INVALID_EMBEDDING_RESPONSE", Message: "Google returned an empty embedding."}
	}
	for i, entry := range payload.Embeddings {
		if len(entry.Values) != dimensions {
			return nil, &llm.Error{Provider: p.Name(), Code: "INVALID_EMBEDDING_RESPONSE", Message: "Google returned vectors with inconsistent dimensions."}
		}
		vectors[i] = entry.Values
	}
	return &llm.EmbeddingResult{Vectors: vectors, Model: model}, nil
}

var liveDrop = regexp.MustCompile(`(?i)transcribe|translate|robotics`)

// LiveVoiceModels lists models that hold a spoken conversation over the Live
// API. Transcription, translation and robotics streams use the same protocol
// but do not talk.
func (p *Provider) LiveVoiceModels(ctx context.Context) ([]llm.CatalogModel, error) {
	raw, err := p.fetchCatalog(ctx)
	if err != nil {
		return nil, err
	}
	var out []llm.CatalogModel
	for _, m := range raw {
		name, _ := m["name"].(string)
		if !supports(m, "bidiGenerateContent") || liveDrop.MatchString(name) {
			continue
		}
		id := shortName(m)
		thinking, _ := m["thinking"].(bool)
		out = append(out, llm.CatalogModel{Model: llm.Model{ID: id, Name: displayName(m, id)}, Thinking: thinking, Raw: m})
	}
	return out, nil
}
