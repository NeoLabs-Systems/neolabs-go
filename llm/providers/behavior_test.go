package providers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NeoLabs-Systems/neolabs-go/llm"
	"github.com/NeoLabs-Systems/neolabs-go/llm/google"
	"github.com/NeoLabs-Systems/neolabs-go/llm/ollama"
	"github.com/NeoLabs-Systems/neolabs-go/llm/transport"
)

// route serves a fixed JSON/text reply per path and records every request.
type route struct {
	status int
	ctype  string
	body   string
	header map[string]string
}

type recorder struct {
	mu   sync.Mutex
	seen []string // "METHOD path"
	auth []string
	body []string
}

func serve(t *testing.T, routes map[string]route) (*httptest.Server, *recorder) {
	t.Helper()
	rec := &recorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		rec.mu.Lock()
		rec.seen = append(rec.seen, r.Method+" "+r.URL.RequestURI())
		rec.auth = append(rec.auth, r.Header.Get("Authorization"))
		rec.body = append(rec.body, string(b))
		rec.mu.Unlock()
		rt, ok := routes[r.URL.RequestURI()]
		if !ok {
			rt, ok = routes[r.Method+" "+r.URL.Path]
		}
		if !ok {
			rt, ok = routes[r.URL.Path]
		}
		if !ok {
			http.NotFound(w, r)
			return
		}
		for k, v := range rt.header {
			w.Header().Set(k, v)
		}
		ct := rt.ctype
		if ct == "" {
			ct = "application/json"
		}
		w.Header().Set("Content-Type", ct)
		if rt.status != 0 {
			w.WriteHeader(rt.status)
		}
		io.WriteString(w, rt.body)
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

func cfgFor(srv *httptest.Server, path string) Config {
	return Config{Config: transport.Config{APIKey: "k", BaseURL: srv.URL + path, HTTPClient: srv.Client()}}
}

func ids(models []llm.Model) []string {
	var out []string
	for _, m := range models {
		out = append(out, m.ID)
	}
	return out
}

func TestOpenAICatalogFiltersAndLiveModels(t *testing.T) {
	srv, _ := serve(t, map[string]route{"/v1/models": {body: `{"data":[{"id":"gpt-5","created":5},{"id":"text-embedding-3-small"},{"id":"whisper-1"},{"id":"gpt-live-1"},{"id":"gpt-live-transcribe"},{"id":"o3-mini"},{"id":"gpt-4.1-instruct"}]}`}})
	p, _ := New("openai", cfgFor(srv, "/v1"))
	models, err := p.Models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(ids(models), ","); got != "gpt-5,o3-mini" {
		t.Errorf("models = %s", got)
	}
	live, err := p.(llm.LiveVoiceLister).LiveVoiceModels(context.Background())
	if err != nil || len(live) != 1 || live[0].ID != "gpt-live-1" {
		t.Errorf("live = %+v, %v", live, err)
	}
	cat, _ := p.(llm.Cataloger).Catalog(context.Background())
	if cat[0].Created != 5 {
		t.Errorf("created = %d", cat[0].Created)
	}
}

func TestListErrorsAreWrappedLikeNode(t *testing.T) {
	srv, _ := serve(t, map[string]route{"/v1/models": {status: 401, body: `{"error":{"message":"bad key sk-abc","code":"invalid_api_key"}}`}})
	for id, want := range map[string]string{
		"openai":            "Failed to list OpenAI models: 401 bad key sk-abc",
		"openai-compatible": "Failed to list custom OpenAI-compatible models: 401 bad key sk-abc",
		"grok":              "Failed to list Grok models: 401 bad key sk-abc",
		"nvidia":            "NVIDIA NIM request failed: 401 bad key sk-abc",
	} {
		p, _ := New(id, cfgFor(srv, "/v1"))
		_, err := p.Models(context.Background())
		if err == nil || err.Error() != want {
			t.Errorf("%s: %v, want %q", id, err, want)
		}
		if e := llm.AsError(err); e == nil || e.Status != 401 || e.Retryable {
			t.Errorf("%s: classification %+v", id, e)
		}
	}
}

func TestOpenRouterCatalogAndDecisions(t *testing.T) {
	srv, _ := serve(t, map[string]route{
		"/api/v1/models": {body: `{"data":[
			{"id":"a/chat","name":"Chat","context_length":32000,"reasoning":{"default_enabled":true,"supported_efforts":["low","high"]},"architecture":{"output_modalities":["text"]}},
			{"id":"b/jev","architecture":{"output_modalities":["decisions"]}}]}`},
		"/api/v1/models?output_modalities=decisions": {body: `{"data":[{"id":"b/jev","architecture":{"output_modalities":["decisions"]}}]}`},
		"/api/v1/systemone":                          {body: `{"model":"b/jev","answers":{"q1":{"yes":0.9}},"usage":{"tokens":3}}`},
	})
	caches := NewCaches()
	cfg := cfgFor(srv, "/api/v1")
	cfg.Caches = caches
	p, _ := New("openrouter", cfg)
	models, err := p.Models(context.Background())
	if err != nil || len(models) != 1 || models[0].ID != "a/chat" || models[0].Name != "Chat" || models[0].ContextWindow != 32000 {
		t.Fatalf("models = %+v, %v", models, err)
	}
	if got := p.ContextWindow("a/chat"); got != 32000 {
		t.Errorf("window = %d", got)
	}
	if got := p.ContextWindow("unknown"); got != 128000 {
		t.Errorf("window = %d", got)
	}
	dec := p.(llm.Decider)
	dm, err := dec.DecisionModels(context.Background())
	if err != nil || len(dm) != 1 || dm[0].ID != "b/jev" {
		t.Fatalf("decision models = %+v, %v", dm, err)
	}
	out, err := dec.Decide(context.Background(), llm.DecisionRequest{Model: "b/jev", State: map[string]any{"s": 1}, Questions: map[string]any{"q1": map[string]any{"type": "noul"}}})
	if err != nil || out["model"] != "b/jev" || out["answers"].(map[string]any)["q1"] == nil {
		t.Fatalf("decide = %v, %v", out, err)
	}
}

func TestNeoRouterCatalog(t *testing.T) {
	srv, rec := serve(t, map[string]route{
		"/v1/models": {body: `{"data":[
			{"id":"auto","name":"Auto (Smartest, free models only)"},
			{"id":"auto-free:fast","name":"Fast"},
			{"id":"m/one","name":"One","context_length":64000,"neorouter":{"tools":true},"pricing":{"prompt":"0.000002"}},
			{"id":"m/free","neorouter":{"free":true}}]}`},
	})
	p, _ := New("neorouter", cfgFor(srv, ""))
	cat, err := p.(llm.Cataloger).Catalog(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, m := range cat {
		got = append(got, m.ID+"|"+m.Name+"|"+m.PriceTier)
	}
	want := "auto:default|NeoRouter Auto · Smartest · free models only|,auto-free:fast|NeoRouter Fast|,m/one|One|medium,m/free|m/free|free"
	if strings.Join(got, ",") != want {
		t.Errorf("catalog\n got: %s\nwant: %s", strings.Join(got, ","), want)
	}
	if !strings.Contains(rec.seen[0], "type=chat") || !p.SupportsVision() || p.ContextWindow("m/one") != 64000 {
		t.Errorf("request %v vision %v window %d", rec.seen, p.SupportsVision(), p.ContextWindow("m/one"))
	}
}

func TestNeoRouterAddsV1AndSurfacesErrors(t *testing.T) {
	srv, rec := serve(t, map[string]route{"/v1/models": {status: 403, body: `{"error":{"message":"key not allowed"}}`}})
	cfg := cfgFor(srv, "/")
	p, _ := New("neorouter", cfg)
	_, err := p.Models(context.Background())
	if err == nil || err.Error() != "NeoRouter: key not allowed" || llm.AsError(err).Status != 403 {
		t.Errorf("err = %v", err)
	}
	if !strings.HasPrefix(rec.seen[0], "GET /v1/models") {
		t.Errorf("path = %v", rec.seen)
	}
}

func TestGoogleCatalogPagingFiltersAndCaches(t *testing.T) {
	var page atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-goog-api-key") != "k" {
			t.Error("missing key")
		}
		if page.Add(1) == 1 {
			io.WriteString(w, `{"models":[{"name":"models/gemini-2.5-pro","displayName":"Gemini Pro","inputTokenLimit":2000000,"thinking":true,"supportedGenerationMethods":["generateContent"]},
				{"name":"models/gemini-tts","supportedGenerationMethods":["generateContent"]},
				{"name":"models/gemini-2-image","supportedGenerationMethods":["generateContent"]},
				{"name":"models/gemini-live","thinking":true,"supportedGenerationMethods":["bidiGenerateContent"]}],"nextPageToken":"p2"}`)
			return
		}
		if r.URL.Query().Get("pageToken") != "p2" {
			t.Errorf("token = %q", r.URL.Query().Get("pageToken"))
		}
		io.WriteString(w, `{"models":[{"name":"models/gemma-3","supportedGenerationMethods":["generateContent"]},{"name":"models/live-transcribe","supportedGenerationMethods":["bidiGenerateContent"]}]}`)
	}))
	defer srv.Close()
	caches := NewCaches()
	cfg := Config{Config: transport.Config{APIKey: "k", BaseURL: srv.URL, HTTPClient: srv.Client()}, Caches: caches}
	p, _ := New("google", cfg)
	models, err := p.Models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(ids(models), ","); got != "gemini-2.5-pro,gemma-3" {
		t.Errorf("models = %s", got)
	}
	if p.ContextWindow("gemini-2.5-pro") != 2000000 || p.ContextWindow("nope") != 1048576 {
		t.Error("context windows not cached")
	}
	page.Store(0)
	live, err := p.(llm.LiveVoiceLister).LiveVoiceModels(context.Background())
	if err != nil || len(live) != 1 || live[0].ID != "gemini-live" || !live[0].Thinking {
		t.Errorf("live = %+v, %v", live, err)
	}
}

func TestGoogleThinkingModelsGetEffortAndUnsignedCallsBecomeText(t *testing.T) {
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if strings.HasSuffix(r.URL.Path, ":generateContent") {
			bodies = append(bodies, string(b))
			io.WriteString(w, `{"candidates":[{"content":{"parts":[{"text":"ok"}]}}]}`)
			return
		}
		io.WriteString(w, `{"models":[{"name":"models/gemini-2.5-pro","thinking":true,"supportedGenerationMethods":["generateContent"]},{"name":"models/gemini-3-pro","thinking":true,"supportedGenerationMethods":["generateContent"]}]}`)
	}))
	defer srv.Close()
	p, _ := New("google", Config{Config: transport.Config{APIKey: "k", BaseURL: srv.URL, HTTPClient: srv.Client()}})
	if _, err := p.Models(context.Background()); err != nil {
		t.Fatal(err)
	}
	msgs := []llm.Message{
		{Role: "user", Content: "hi"},
		{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "x1", Type: "function", Function: llm.FunctionCall{Name: "f", Arguments: `{"a":1}`}}}},
		{Role: "tool", Name: "f", ToolCallID: "x1", Content: "done"},
	}
	for model, want := range map[string]string{"gemini-2.5-pro": `"thinkingConfig":{"thinkingBudget":24576}`, "gemini-3-pro": `"thinkingConfig":{"thinkingLevel":"HIGH"}`} {
		bodies = nil
		if _, err := p.Chat(context.Background(), msgs, nil, llm.Options{Model: model, ReasoningEffort: "high"}); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(bodies[0], want) {
			t.Errorf("%s body lacks %s: %s", model, want, bodies[0])
		}
		if strings.Contains(bodies[0], "functionCall") || !strings.Contains(bodies[0], "[Called f with {\\\"a\\\":1}]") || !strings.Contains(bodies[0], "[Result of f]\\ndone") {
			t.Errorf("%s: unsigned call not passed as text: %s", model, bodies[0])
		}
	}
	// a signed turn keeps its functionCall
	bodies = nil
	msgs[1].ReasoningState = map[string]any{google.ThoughtSignaturesKey: map[string]any{"x1": "sig"}}
	if _, err := p.Chat(context.Background(), msgs, nil, llm.Options{Model: "gemini-2.5-pro"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(bodies[0], `"functionCall":{"args":{"a":1},"id":"x1","name":"f"},"thoughtSignature":"sig"`) {
		t.Errorf("signed call lost: %s", bodies[0])
	}
}

func TestOllamaCatalogDecisionModelsAndPull(t *testing.T) {
	var pulled atomic.Bool
	var statuses []ollama.Status
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			if pulled.Load() {
				io.WriteString(w, `{"models":[{"name":"new:latest"}]}`)
				return
			}
			io.WriteString(w, `{"models":[{"name":"llama3:latest"},{"name":"nimble","capabilities":["decision"]}]}`)
		case "/api/pull":
			b, _ := io.ReadAll(r.Body)
			if string(b) != `{"name":"new","stream":false}` {
				t.Errorf("pull body = %s", b)
			}
			pulled.Store(true)
			io.WriteString(w, `{"status":"success"}`)
		case "/api/chat":
			io.WriteString(w, `{"model":"new:latest","message":{"role":"assistant","content":"hi"},"done":true}`)
		}
	}))
	defer srv.Close()
	cfg := cfgFor(srv, "")
	cfg.OnStatus = func(s ollama.Status) { mu.Lock(); statuses = append(statuses, s); mu.Unlock() }
	p, _ := New("ollama", cfg)
	models, _ := p.Models(context.Background())
	if strings.Join(ids(models), ",") != "llama3:latest" {
		t.Errorf("chat models = %v", ids(models))
	}
	dm, _ := p.(llm.Decider).DecisionModels(context.Background())
	if len(dm) != 1 || dm[0].ID != "nimble" {
		t.Errorf("decision models = %v", dm)
	}
	res, err := p.Chat(context.Background(), []llm.Message{{Role: "user", Content: "x"}}, nil, llm.Options{Model: "new"})
	if err != nil || res.Content != "hi" {
		t.Fatalf("chat = %+v, %v", res, err)
	}
	if len(statuses) != 2 || statuses[0].Status != "started" || statuses[1].Status != "completed" || statuses[1].Phase != "Thinking" {
		t.Errorf("statuses = %+v", statuses)
	}
}

func TestOllamaUnreachableCatalogIsEmptyAndPullFailureReported(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/pull" {
			w.WriteHeader(500)
			io.WriteString(w, `{"error":"registry down"}`)
			return
		}
		http.Error(w, "boom", 500)
	}))
	defer srv.Close()
	var statuses []ollama.Status
	cfg := cfgFor(srv, "")
	cfg.OnStatus = func(s ollama.Status) { statuses = append(statuses, s) }
	p, _ := New("ollama", cfg)
	if models, err := p.Models(context.Background()); err != nil || len(models) != 0 {
		t.Errorf("models = %v, %v", models, err)
	}
	_, err := p.Chat(context.Background(), nil, nil, llm.Options{Model: "m"})
	if err == nil || err.Error() != "Pull failed with HTTP 500: registry down" || !llm.IsTransient(err) {
		t.Errorf("err = %v", err)
	}
	if len(statuses) != 2 || statuses[1].Status != "failed" || !strings.Contains(statuses[1].Message, "registry down") {
		t.Errorf("statuses = %+v", statuses)
	}
}

func TestTypeSafeModelsAndDecisions(t *testing.T) {
	srv, rec := serve(t, map[string]route{
		"/v1/models":    {body: `{"models":[{"name":"jev-1","release_date":"x"},{"nope":1}]}`},
		"/v1/systemone": {body: `{"answers":{"a":1}}`},
	})
	p, _ := New("typesafe", cfgFor(srv, "/v1"))
	dm, err := p.(llm.Decider).DecisionModels(context.Background())
	if err != nil || len(dm) != 1 || dm[0] != (llm.Model{ID: "jev-1", Name: "jev-1"}) {
		t.Fatalf("decision models = %v, %v", dm, err)
	}
	if chat, _ := p.Models(context.Background()); len(chat) != 0 {
		t.Error("typesafe has no chat models")
	}
	out, err := p.(llm.Decider).Decide(context.Background(), llm.DecisionRequest{Model: "jev-1", State: map[string]any{"x": 1}, Questions: map[string]any{"a": map[string]any{"type": "noul"}}})
	if err != nil || out["model"] != "jev-1" {
		t.Fatalf("decide = %v, %v", out, err)
	}
	if rec.body[1] != `{"model":"jev-1","state":{"x":1},"questions":{"a":{"type":"noul"}}}` || rec.auth[1] != "Bearer k" {
		t.Errorf("request = %s auth %s", rec.body[1], rec.auth[1])
	}
	if _, err := p.Chat(context.Background(), nil, nil, llm.Options{}); err == nil || err.Error() != "chat() not implemented" {
		t.Errorf("chat err = %v", err)
	}
}

func TestSystemOneErrors(t *testing.T) {
	for name, tc := range map[string]struct {
		route route
		want  string
	}{
		"http error": {route{status: 429, body: `{"error":{"message":"slow down"}}`}, "jev returned HTTP 429: slow down"},
		"string err": {route{status: 400, body: `{"error":"bad state"}`}, "jev returned HTTP 400: bad state"},
		"no answers": {route{body: `{"model":"jev"}`}, "jev returned no answers."},
		"not json":   {route{status: 502, body: `<html>`}, "jev returned HTTP 502"},
	} {
		srv, _ := serve(t, map[string]route{"/v1/systemone": tc.route})
		p, _ := New("typesafe", cfgFor(srv, "/v1"))
		_, err := p.(llm.Decider).Decide(context.Background(), llm.DecisionRequest{Model: "jev"})
		if err == nil || err.Error() != tc.want {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
	}
	// a deadline reads like Node's fetch wrapper
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { time.Sleep(300 * time.Millisecond) }))
	defer slow.Close()
	p, _ := New("typesafe", cfgFor(slow, ""))
	_, err := p.(llm.Decider).Decide(context.Background(), llm.DecisionRequest{Model: "jev", Timeout: 50 * time.Millisecond})
	if err == nil || err.Error() != "SystemOne decision request timed out after 50ms." || !llm.IsTransient(err) {
		t.Errorf("timeout err = %v", err)
	}
}

func TestDefaultClientRefusesInternalAddresses(t *testing.T) {
	srv, _ := serve(t, map[string]route{"/v1/models": {body: `{"data":[]}`}})
	// no HTTPClient, not trusted: a user-supplied base URL must not reach loopback
	p, _ := New("openai-compatible", Config{Config: transport.Config{APIKey: "k", BaseURL: srv.URL + "/v1"}})
	_, err := p.Models(context.Background())
	if err == nil || !strings.Contains(err.Error(), "destination is not allowed") {
		t.Errorf("err = %v", err)
	}
	if llm.IsTransient(err) {
		t.Error("a refused destination is not transient")
	}
	// the operator's own address may be private
	p, _ = New("openai-compatible", Config{Config: transport.Config{APIKey: "k", BaseURL: srv.URL + "/v1", TrustedBaseURL: true}})
	if _, err := p.Models(context.Background()); err != nil {
		t.Errorf("trusted: %v", err)
	}
	// Ollama on localhost is the default and is allowed
	o, _ := New("ollama", Config{Config: transport.Config{BaseURL: srv.URL, TrustedBaseURL: true}})
	if _, err := o.Models(context.Background()); err != nil {
		t.Errorf("ollama: %v", err)
	}
	// a user-supplied Ollama address is refused as an empty catalog, not fetched
	o, _ = New("ollama", Config{Config: transport.Config{BaseURL: srv.URL}})
	if models, err := o.Models(context.Background()); err != nil || len(models) != 0 {
		t.Errorf("untrusted ollama = %v, %v", models, err)
	}
}

func TestStreamGuardStopsRepetitionAndCancellationStopsStream(t *testing.T) {
	chunk := func(text string) string {
		b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": text}}}})
		return "data: " + string(b) + "\n\n"
	}
	srv, _ := serve(t, map[string]route{"/v1/chat/completions": {ctype: "text/event-stream", body: chunk(strings.Repeat(" ", 600)) + chunk(strings.Repeat(" ", 600)) + chunk("never")}})
	p, _ := New("openai-compatible", cfgFor(srv, "/v1"))
	var content string
	var gotErr error
	for ev, err := range p.Stream(context.Background(), nil, nil, llm.Options{Model: "m"}) {
		if err != nil {
			gotErr = err
			break
		}
		content += ev.Content
	}
	d, ok := llm.AsDegenerateOutput(gotErr)
	if !ok || d.Reason != "degenerate_repetition" || d.OutputBytes != 1200 {
		t.Fatalf("err = %v (%+v)", gotErr, d)
	}
	if want := "Custom OpenAI-compatible stream failed: openai-compatible stream aborted: degenerate_repetition after 1200 bytes of output."; gotErr.Error() != want {
		t.Errorf("message = %q, want %q", gotErr.Error(), want)
	}

	started := make(chan struct{})
	hang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, chunk("hello"))
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
	}))
	defer hang.Close()
	p, _ = New("openai-compatible", cfgFor(hang, "/v1"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var last error
	for ev, err := range p.Stream(ctx, nil, nil, llm.Options{Model: "m"}) {
		if err != nil {
			last = err
			break
		}
		if ev.Content == "hello" {
			<-started
			cancel()
		}
	}
	if last != context.Canceled {
		t.Errorf("err after cancel = %v", last)
	}
}

func TestRetryPolicy(t *testing.T) {
	policy := llm.RetryPolicy{MaxAttempts: 3, BaseDelay: time.Millisecond, MaxDelay: 2 * time.Millisecond, MaxRetryAfter: 50 * time.Millisecond}
	calls := 0
	var infos []llm.RetryInfo
	policy.OnRetry = func(i llm.RetryInfo) { infos = append(infos, i) }
	v, err := llm.Retry(context.Background(), policy, func(attempt int) (string, error) {
		calls++
		if attempt < 3 {
			return "", &llm.Error{Status: 503, Message: "503 busy", Retryable: true}
		}
		return "done", nil
	})
	if err != nil || v != "done" || calls != 3 || len(infos) != 2 || infos[0].Attempt != 1 {
		t.Errorf("v=%q err=%v calls=%d infos=%d", v, err, calls, len(infos))
	}
	// non-transient errors are not retried; neither is a provider-requested wait beyond the cap
	calls = 0
	_, err = llm.Retry(context.Background(), policy, func(int) (int, error) { calls++; return 0, &llm.Error{Status: 400, Message: "bad"} })
	if err == nil || calls != 1 {
		t.Errorf("400 retried: calls=%d", calls)
	}
	calls = 0
	_, err = llm.Retry(context.Background(), policy, func(int) (int, error) {
		calls++
		return 0, &llm.Error{Status: 429, Retryable: true, RetryAfter: time.Minute, Message: "429 quota"}
	})
	if err == nil || calls != 1 {
		t.Errorf("long retry-after honoured: calls=%d", calls)
	}
	// exhaustion returns the last error; cancellation during the wait returns the context error
	calls = 0
	_, err = llm.Retry(context.Background(), policy, func(int) (int, error) { calls++; return 0, &llm.Error{Status: 500, Retryable: true, Message: "500"} })
	if err == nil || calls != 3 {
		t.Errorf("exhausted: calls=%d err=%v", calls, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	slow := llm.RetryPolicy{MaxAttempts: 3, BaseDelay: time.Hour, MaxDelay: time.Hour, MaxRetryAfter: time.Hour}
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	_, err = llm.Retry(ctx, slow, func(int) (int, error) { return 0, &llm.Error{Status: 500, Retryable: true} })
	if err != context.Canceled {
		t.Errorf("cancel during wait: %v", err)
	}
	p := llm.RetryPolicyFromEnv(func(k string) string {
		return map[string]string{"NEOAGENT_AI_RETRY_MAX_ATTEMPTS": "99", "NEOAGENT_AI_RETRY_BASE_MS": "100", "NEOAGENT_AI_RETRY_MAX_MS": "bogus"}[k]
	})
	if p.MaxAttempts != 8 || p.BaseDelay != 100*time.Millisecond || p.MaxDelay != 8*time.Second || p.MaxRetryAfter != time.Minute {
		t.Errorf("env policy = %+v", p)
	}
}

func TestStatusErrorCarriesRetryHints(t *testing.T) {
	h := http.Header{}
	h.Set("retry-after", "7")
	e := llm.StatusError("openai", 429, h, []byte(`{"error":{"message":"slow","code":"rate_limit_exceeded"}}`), false)
	if e.Message != "429 slow" || e.Code != "rate_limit_exceeded" || !e.Retryable || e.RetryAfter != 7*time.Second || e.Status != 429 {
		t.Errorf("%+v", e)
	}
	e = llm.StatusError("x", 500, nil, []byte(`upstream exploded`), false)
	if e.Message != "500 upstream exploded" || !e.Retryable {
		t.Errorf("%+v", e)
	}
	e = llm.StatusError("x", 502, nil, nil, false)
	if e.Message != "502 status code (no body)" {
		t.Errorf("%+v", e)
	}
	// an error event with an embedded status is classified by it
	e = llm.StatusError("x", 0, nil, []byte(`{"code":429,"message":"queue full"}`), true)
	if e.Message != "queue full" || e.Status != 429 || !e.Retryable {
		t.Errorf("%+v", e)
	}
}

func TestMissingModelIsExplicitForEveryProvider(t *testing.T) {
	for _, id := range []string{"openai", "openai-compatible", "anthropic", "google", "grok", "nvidia", "nous", "openrouter", "neorouter", "github-copilot", "openai-codex", "claude-code", "ollama", "grok-oauth"} {
		p, err := New(id, Config{Config: transport.Config{APIKey: "k", BaseURL: "http://127.0.0.1:1"}})
		if err != nil {
			t.Fatal(id, err)
		}
		_, err = p.Chat(context.Background(), nil, nil, llm.Options{Model: "  "})
		e := llm.AsError(err)
		if e == nil || e.Code != llm.CodeModelSelectionRequired || !strings.HasPrefix(e.Message, "Provider '") {
			t.Errorf("%s: %v", id, err)
		}
	}
	if _, err := New("nope", Config{}); err == nil || err.Error() != "Unknown provider: nope" {
		t.Errorf("unknown provider: %v", err)
	}
	if len(IDs()) != 16 {
		t.Errorf("ids = %v", IDs())
	}
}
