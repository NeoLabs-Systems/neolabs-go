// Package codex serves OpenAI Codex models through the Responses API: the ChatGPT
// Codex backend (OAuth subscription) or any OpenAI-compatible Responses endpoint.
package codex

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"iter"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/NeoLabs-Systems/neolabs-go/llm"
	"github.com/NeoLabs-Systems/neolabs-go/llm/oauth"
	"github.com/NeoLabs-Systems/neolabs-go/llm/openaichat"
	"github.com/NeoLabs-Systems/neolabs-go/llm/transport"
)

const (
	defaultBaseURL = "https://chatgpt.com/backend-api/codex"
	clientID       = "app_EMoamEEZ73f0CkXaXp7hrann"
	tokenURL       = "https://auth.openai.com/oauth/token"
)

// Config configures the Codex provider. APIKey is the OAuth access token; give
// Credentials instead to share and refresh it.
type Config struct {
	transport.Config
	Credentials *oauth.Credentials
	// Version, InstallationID, Residency and AccountID fill the identifying
	// headers of the Codex backend. AccountID is read from the access token when
	// empty.
	Version        string
	InstallationID string
	Residency      string
	AccountID      string
	// TokenURL replaces the OAuth token endpoint (tests).
	TokenURL string
	// ForceBackend treats the endpoint as the Codex backend whatever its URL
	// (tests against a local fake).
	ForceBackend bool
}

// Provider is the Codex client.
type Provider struct {
	hc      *http.Client
	baseURL string
	backend bool
	headers map[string]string
	version string
	creds   *oauth.Credentials
	refresh func(ctx context.Context, attempted string) (string, error)
}

func hostPath(raw string) (host, path string, ok bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || strings.TrimSpace(raw) == "" || u.Host == "" {
		return "", "", false
	}
	return u.Hostname(), strings.TrimRight(u.Path, "/"), true
}

func isBackendBaseURL(raw string) bool {
	host, path, ok := hostPath(raw)
	if !ok || host != "chatgpt.com" {
		return false
	}
	switch path {
	case "/backend-api", "/backend-api/v1", "/backend-api/codex", "/backend-api/codex/v1":
		return true
	}
	return false
}

func isOpenAIAPIBaseURL(raw string) bool {
	host, path, ok := hostPath(raw)
	return ok && host == "api.openai.com" && (path == "" || path == "/v1")
}

// New returns the Codex client. An empty base URL, the OpenAI API URL or a
// ChatGPT backend URL all mean the Codex backend.
func New(cfg Config) *Provider {
	base := cfg.BaseURL
	if base == "" || isOpenAIAPIBaseURL(base) || isBackendBaseURL(base) {
		base = defaultBaseURL
	}
	backend := isBackendBaseURL(base) || cfg.ForceBackend
	if base == defaultBaseURL {
		cfg.TrustedBaseURL = true
	}
	creds := cfg.Credentials
	if creds == nil {
		creds = oauth.New(oauth.Tokens{Access: cfg.APIKey}, nil)
	}
	version := cfg.Version
	if version == "" {
		version = "unknown"
	}
	p := &Provider{hc: cfg.Client(), baseURL: base, backend: backend, version: version, creds: creds, headers: map[string]string{}}
	for k, v := range cfg.Headers {
		p.headers[k] = v
	}
	if backend {
		accountID := cfg.AccountID
		if accountID == "" {
			claims := oauth.JWTClaims(creds.Get().Access)
			accountID, _ = claims["chatgpt_account_id"].(string)
			if nested, ok := claims["https://api.openai.com/auth"].(map[string]any); ok && accountID == "" {
				accountID, _ = nested["chatgpt_account_id"].(string)
			}
		}
		residency := cfg.Residency
		if residency == "" {
			residency = "us"
		}
		installation := cfg.InstallationID
		if installation == "" {
			installation = newUUID()
		}
		p.headers["originator"] = "openclaw"
		p.headers["version"] = version
		p.headers["User-Agent"] = "openclaw/" + version
		p.headers["x-codex-installation-id"] = installation
		p.headers["x-openai-internal-codex-residency"] = residency
		if accountID != "" {
			p.headers["ChatGPT-Account-Id"] = accountID
		}
	}
	endpoint := cfg.TokenURL
	if endpoint == "" {
		endpoint = tokenURL
	}
	hc := p.hc
	// A 401 renews the access token once: refresh tokens rotate, so concurrent
	// clients share one refresh through the credentials.
	p.refresh = func(ctx context.Context, attempted string) (string, error) {
		if current := p.creds.Get(); current.Access != "" && current.Access != attempted {
			return current.Access, nil
		}
		fresh, err := p.creds.Refresh(ctx, func(ctx context.Context, refreshToken string) (oauth.Tokens, error) {
			return refreshAccessToken(ctx, hc, endpoint, refreshToken)
		})
		return fresh.Access, err
	}
	return p
}

func newUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

func (p *Provider) Name() string { return "openai-codex" }

func (p *Provider) ContextWindow(string) int { return 128000 }

func (p *Provider) SupportsVision() bool { return false }

var reasoningID = regexp.MustCompile(`^o\d|^gpt-(?:[5-9]|\d{2,})(?:[.\-]|$)|reasoning`)

func (p *Provider) isReasoningModel(model string) bool {
	return p.backend || reasoningID.MatchString(strings.ToLower(strings.TrimSpace(model)))
}

func (p *Provider) requestHeaders(accept string, body bool) map[string]string {
	h := map[string]string{"Accept": accept}
	if body {
		h["Content-Type"] = "application/json"
	}
	for k, v := range p.headers {
		h[k] = v
	}
	if token := p.creds.Get().Access; token != "" {
		h["Authorization"] = "Bearer " + token
	}
	if p.backend && body {
		id := newUUID()
		h["x-client-request-id"] = id
		h["x-openclaw-session-id"] = id
		h["x-openclaw-turn-id"] = id
		h["x-openclaw-turn-attempt"] = "1"
	}
	return h
}

// withRefresh runs one request and repeats it once with a renewed token after a 401.
func withRefresh[T any](ctx context.Context, p *Provider, op func() (T, error)) (T, error) {
	attempted := p.creds.Get().Access
	value, err := op()
	if e := llm.AsError(err); e != nil && e.Status == 401 && p.creds.Refreshable() {
		if token, rerr := p.refresh(ctx, attempted); rerr == nil && token != "" {
			return op()
		}
	}
	return value, err
}

// requestError words a failed call the way the Codex provider reports it: the
// SDK message plus status, type, code, param and request id.
func (p *Provider) requestError(resp *http.Response, prefix string) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	resp.Body.Close()
	e := llm.StatusError("openai-codex", resp.StatusCode, resp.Header, body, false)
	parts := []string{fmt.Sprintf("HTTP %d", resp.StatusCode)}
	if v, err := transport.ParseOrdered(body); err == nil {
		if doc, ok := v.(*transport.Obj); ok {
			if errObj := doc.Object("error"); errObj != nil {
				for _, key := range []string{"type", "code", "param"} {
					if v, _ := errObj.Get(key); v != nil && v != "" {
						parts = append(parts, fmt.Sprintf("%s=%s", key, scalar(v)))
					}
				}
			}
		}
	}
	if id := resp.Header.Get("x-request-id"); id != "" {
		parts = append(parts, "request_id="+id)
	}
	return llm.WrapErrorDetail("openai-codex", e, prefix, fmt.Sprintf("%s (%s)", e.Message, strings.Join(parts, ", ")))
}

func scalar(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	b, _ := transport.Marshal(v)
	return string(b)
}

func (p *Provider) post(ctx context.Context, body *transport.Obj) (*http.Response, error) {
	payload, err := transport.Marshal(body)
	if err != nil {
		return nil, err
	}
	return withRefresh(ctx, p, func() (*http.Response, error) {
		resp, err := transport.Do(ctx, p.hc, "openai-codex", "Connection error.", transport.Request{
			Method: http.MethodPost, URL: transport.JoinURL(p.baseURL, "/responses"), Header: p.requestHeaders("application/json", true), Body: payload,
		})
		if err != nil {
			return nil, err
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			// Keep the failure as an *llm.Error with its status for the refresh
			// decision, worded for the caller.
			return nil, p.requestError(resp, "OpenAI Codex request failed")
		}
		return resp, nil
	})
}

// Chat runs one request. The Codex backend only streams, so there the answer is
// collected from the stream.
func (p *Provider) Chat(ctx context.Context, msgs []llm.Message, tools []llm.Tool, opt llm.Options) (*llm.Result, error) {
	if p.backend {
		var final llm.Event
		var content strings.Builder
		for ev, err := range p.Stream(ctx, msgs, tools, opt) {
			if err != nil {
				return nil, err
			}
			switch ev.Type {
			case llm.EventContent:
				content.WriteString(ev.Content)
			case llm.EventToolCalls, llm.EventDone:
				final = ev
			}
		}
		model, err := openaichat.RequireModel("openai-codex", opt)
		if err != nil {
			return nil, err
		}
		result := &llm.Result{Content: final.Content, ToolCalls: final.ToolCalls, FinishReason: final.FinishReason, Usage: final.Usage, ReasoningState: final.ReasoningState, Model: model}
		if result.Content == "" {
			result.Content = content.String()
		}
		if result.FinishReason == "" {
			result.FinishReason = "stop"
			if len(result.ToolCalls) > 0 {
				result.FinishReason = "tool_calls"
			}
		}
		if final.Model != "" {
			result.Model = final.Model
		}
		return result, nil
	}
	model, err := openaichat.RequireModel("openai-codex", opt)
	if err != nil {
		return nil, err
	}
	body := transport.NewObj().Set("model", model)
	req := p.buildRequest(msgs, tools, opt, model)
	for _, k := range req.Keys() {
		v, _ := req.Get(k)
		body.Set(k, v)
	}
	resp, err := p.post(ctx, body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, &llm.Error{Provider: "openai-codex", Message: "Connection error.", Retryable: true}
	}
	parsed, err := transport.ParseOrdered(raw)
	doc, _ := parsed.(*transport.Obj)
	if err != nil || doc == nil {
		return nil, llm.WrapError("openai-codex", &llm.Error{Provider: "openai-codex", Message: "Unexpected token in JSON response"}, "OpenAI Codex request failed")
	}
	calls := extractToolCalls(doc)
	finish := "stop"
	if len(calls) > 0 {
		finish = "tool_calls"
	}
	return &llm.Result{
		Content: responseText(doc), ReasoningState: extractReasoningState(doc), ToolCalls: calls,
		FinishReason: finish, Usage: responsesUsage(doc), Model: doc.String("model"),
	}, nil
}

// Stream yields output text deltas and a final tool_calls or done event.
func (p *Provider) Stream(ctx context.Context, msgs []llm.Message, tools []llm.Tool, opt llm.Options) iter.Seq2[llm.Event, error] {
	return func(yield func(llm.Event, error) bool) {
		model, err := openaichat.RequireModel("openai-codex", opt)
		if err != nil {
			yield(llm.Event{}, err)
			return
		}
		body := transport.NewObj().Set("model", model)
		req := p.buildRequest(msgs, tools, opt, model)
		for _, k := range req.Keys() {
			v, _ := req.Get(k)
			body.Set(k, v)
		}
		body.Set("stream", true)
		resp, err := p.post(ctx, body)
		if err != nil {
			yield(llm.Event{}, err)
			return
		}
		defer resp.Body.Close()

		var content strings.Builder
		var finalResponse *transport.Obj
		var streamed []any
		stopped := false
		err = transport.ReadSSE(resp.Body, func(ev transport.SSEEvent) error {
			if ev.Data == "" || ev.Data == "[DONE]" {
				return nil
			}
			v, perr := transport.ParseOrdered([]byte(ev.Data))
			event, _ := v.(*transport.Obj)
			if perr != nil || event == nil {
				return &llm.Error{Provider: "openai-codex", Message: "Could not parse stream event as JSON"}
			}
			if errObj := event.Object("error"); errObj != nil {
				return llm.StatusError("openai-codex", 0, nil, mustMarshal(errObj), true)
			}
			switch event.String("type") {
			case "response.output_text.delta":
				if delta, ok := event.Get("delta"); ok {
					if text, ok := delta.(string); ok {
						content.WriteString(text)
						if !yield(llm.Event{Type: llm.EventContent, Content: text}, nil) {
							stopped = true
							return io.EOF
						}
					}
				}
			case "response.output_item.done":
				if item := event.Object("item"); item != nil {
					streamed = append(streamed, item)
				}
			case "response.completed":
				finalResponse = event.Object("response")
			case "response.failed":
				failure := "Codex response failed"
				if r := event.Object("response"); r != nil {
					if e := r.Object("error"); e != nil && e.String("message") != "" {
						failure = e.String("message")
					}
				}
				return &llm.Error{Provider: "openai-codex", Message: failure}
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
				yield(llm.Event{}, &llm.Error{Provider: "openai-codex", Message: "terminated", Retryable: true})
			}
			return
		}
		// The Codex backend streams each output item but sends response.completed
		// with an empty output, so tool calls only survive if collected here.
		response := transport.NewObj()
		if finalResponse != nil {
			response = finalResponse.Clone()
		}
		if len(response.List("output")) == 0 {
			response.Set("output", streamed)
		}
		calls := extractToolCalls(response)
		finalContent := responseText(response)
		if finalContent == "" {
			finalContent = content.String()
		}
		if len(calls) > 0 {
			yield(llm.Event{Type: llm.EventToolCalls, Content: finalContent, ToolCalls: calls, ReasoningState: extractReasoningState(response), Usage: responsesUsage(response)}, nil)
			return
		}
		yield(llm.Event{Type: llm.EventDone, Content: finalContent, FinishReason: "stop", Usage: responsesUsage(response)}, nil)
	}
}

func mustMarshal(v any) []byte {
	b, _ := transport.Marshal(v)
	return b
}

// Models lists the Codex catalog (or the OpenAI model list on a custom endpoint).
func (p *Provider) Models(ctx context.Context) ([]llm.Model, error) {
	path := "/models"
	key := "data"
	if p.backend {
		path += "?client_version=" + url.QueryEscape(p.version)
		key = "models"
	}
	doc, err := withRefresh(ctx, p, func() (*transport.Obj, error) {
		resp, err := transport.Do(ctx, p.hc, "openai-codex", "Connection error.", transport.Request{
			URL: transport.JoinURL(p.baseURL, path), Header: p.requestHeaders("application/json", false),
		})
		if err != nil {
			return nil, err
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
			resp.Body.Close()
			return nil, llm.StatusError("openai-codex", resp.StatusCode, resp.Header, body, false)
		}
		defer resp.Body.Close()
		raw, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, &llm.Error{Provider: "openai-codex", Message: "Connection error.", Retryable: true}
		}
		v, perr := transport.ParseOrdered(raw)
		doc, _ := v.(*transport.Obj)
		if perr != nil || doc == nil {
			return nil, &llm.Error{Provider: "openai-codex", Message: "Unexpected token in JSON response"}
		}
		return doc, nil
	})
	if err != nil {
		return nil, llm.WrapError("openai-codex", err, "Failed to list OpenAI Codex models")
	}
	var out []llm.Model
	for _, item := range doc.List(key) {
		m, _ := item.(*transport.Obj)
		if m == nil {
			continue
		}
		if v, ok := m.Get("supported_in_api"); ok && v == false {
			continue
		}
		if m.String("visibility") == "hide" {
			continue
		}
		id := m.String("slug")
		if id == "" {
			id = m.String("id")
		}
		name := m.String("display_name")
		if name == "" {
			name = m.String("id")
		}
		if name == "" {
			name = m.String("slug")
		}
		if id != "" {
			out = append(out, llm.Model{ID: id, Name: name, ContextWindow: 128000})
		}
	}
	return out, nil
}

func refreshAccessToken(ctx context.Context, hc *http.Client, endpoint, refreshToken string) (oauth.Tokens, error) {
	body := mustMarshal(transport.NewObj().Set("client_id", clientID).Set("grant_type", "refresh_token").Set("refresh_token", refreshToken).Set("scope", "openid profile email"))
	res, err := transport.FetchText(ctx, hc, transport.FetchOptions{
		Method: http.MethodPost, URL: endpoint, Body: body,
		Header:   map[string]string{"Content-Type": "application/json", "Accept": "application/json"},
		MaxBytes: 256 * 1024, Timeout: 30 * time.Second, Service: "OpenAI Codex OAuth refresh", Provider: "openai-codex",
	})
	if err != nil {
		return oauth.Tokens{}, err
	}
	var data map[string]any
	if res.Text != "" {
		if v, perr := transport.ParseOrdered([]byte(res.Text)); perr == nil {
			data, _ = toMap(v)
		}
	}
	if !res.OK() {
		detail := ""
		if e, ok := data["error"].(map[string]any); ok {
			detail, _ = e["message"].(string)
		}
		for _, v := range []any{data["error_description"], data["error"], res.Text} {
			if detail != "" {
				break
			}
			detail, _ = v.(string)
		}
		return oauth.Tokens{}, fmt.Errorf("HTTP %d %s", res.Status, llm.SanitizeDetail(detail))
	}
	access, _ := data["access_token"].(string)
	if access == "" {
		return oauth.Tokens{}, fmt.Errorf("Refresh response did not include an access token.")
	}
	refresh, _ := data["refresh_token"].(string)
	if refresh == "" {
		refresh = refreshToken
	}
	return oauth.Tokens{Access: access, Refresh: refresh}, nil
}

func toMap(v any) (map[string]any, bool) {
	o, ok := v.(*transport.Obj)
	if !ok {
		return nil, false
	}
	m := map[string]any{}
	for _, k := range o.Keys() {
		val, _ := o.Get(k)
		if nested, ok := val.(*transport.Obj); ok {
			val, _ = toMap(nested)
		}
		m[k] = val
	}
	return m, true
}
