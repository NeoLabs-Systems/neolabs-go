package providers

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NeoLabs-Systems/neolabs-go/llm"
	"github.com/NeoLabs-Systems/neolabs-go/llm/anthropic"
	"github.com/NeoLabs-Systems/neolabs-go/llm/codex"
	"github.com/NeoLabs-Systems/neolabs-go/llm/oauth"
	"github.com/NeoLabs-Systems/neolabs-go/llm/openaichat"
	"github.com/NeoLabs-Systems/neolabs-go/llm/transport"
)

const chatOK = `{"choices":[{"finish_reason":"stop","message":{"content":"ok"}}]}`

func jwt(claims string) string {
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"none"}`)) + "." + enc([]byte(claims)) + ".sig"
}

func chat(t *testing.T, p llm.Provider) error {
	t.Helper()
	_, err := p.Chat(context.Background(), []llm.Message{{Role: "user", Content: "hi"}}, nil, llm.Options{Model: "m"})
	return err
}

func TestGrokOAuthRefreshesBeforeExpiryAndAfterRejection(t *testing.T) {
	var tokenForms []url.Values
	var persisted []oauth.Tokens
	var mu sync.Mutex
	var rejectOnce atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/token":
			b, _ := io.ReadAll(r.Body)
			form, _ := url.ParseQuery(string(b))
			tokenForms = append(tokenForms, form)
			io.WriteString(w, `{"access_token":"fresh-`+form.Get("refresh_token")+`","refresh_token":"r-next","expires_in":3600}`)
		case "/v1/chat/completions":
			if rejectOnce.CompareAndSwap(true, false) {
				w.WriteHeader(401)
				io.WriteString(w, `{"error":{"message":"expired"}}`)
				return
			}
			w.Header().Set("X-Seen-Auth", r.Header.Get("Authorization"))
			io.WriteString(w, chatOK)
		}
	}))
	defer srv.Close()

	creds := oauth.New(oauth.Tokens{Access: "old", Refresh: "r1", ExpiresAtMs: time.Now().Add(time.Minute).UnixMilli()}, func(tk oauth.Tokens) { persisted = append(persisted, tk) })
	p := openaichat.NewGrokOAuth(openaichat.GrokOAuthConfig{
		Config:      transport.Config{BaseURL: srv.URL + "/v1", HTTPClient: srv.Client()},
		Credentials: creds, TokenURL: srv.URL + "/token",
	})
	if err := chat(t, p); err != nil {
		t.Fatal(err)
	}
	// near expiry: refreshed first, form-encoded like Node
	if len(tokenForms) != 1 || tokenForms[0].Get("grant_type") != "refresh_token" || tokenForms[0].Get("refresh_token") != "r1" || tokenForms[0].Get("client_id") != "b1a00492-073a-47ea-816f-4c329264a828" {
		t.Fatalf("refresh requests = %v", tokenForms)
	}
	if got := creds.Get(); got.Access != "fresh-r1" || got.Refresh != "r-next" || got.ExpiresAtMs < time.Now().Add(50*time.Minute).UnixMilli() {
		t.Errorf("credentials = %+v", got)
	}
	if len(persisted) != 1 || persisted[0].Access != "fresh-r1" {
		t.Errorf("persisted = %+v", persisted)
	}
	// fresh token: no further refresh. A 401 triggers exactly one refresh and a retry.
	if err := chat(t, p); err != nil || len(tokenForms) != 1 {
		t.Fatalf("second chat: %v, refreshes %d", err, len(tokenForms))
	}
	rejectOnce.Store(true)
	if err := chat(t, p); err != nil {
		t.Fatal(err)
	}
	if len(tokenForms) != 2 || tokenForms[1].Get("refresh_token") != "r-next" || creds.Get().Access != "fresh-r-next" {
		t.Errorf("after 401: refreshes %v, creds %+v", tokenForms, creds.Get())
	}
}

func TestGrokOAuthRefreshFailureTexts(t *testing.T) {
	status, body := 400, `{"error":"invalid_grant"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			w.WriteHeader(status)
			io.WriteString(w, body)
			return
		}
		io.WriteString(w, chatOK)
	}))
	defer srv.Close()
	build := func() llm.Provider {
		creds := oauth.New(oauth.Tokens{Access: "old", Refresh: "r1", ExpiresAtMs: time.Now().UnixMilli()}, nil)
		return openaichat.NewGrokOAuth(openaichat.GrokOAuthConfig{Config: transport.Config{BaseURL: srv.URL + "/v1", HTTPClient: srv.Client()}, Credentials: creds, TokenURL: srv.URL + "/token"})
	}
	if err := chat(t, build()); err == nil || err.Error() != "Grok OAuth refresh token is invalid or expired. Re-run `neoagent login grok-oauth` to re-authenticate." {
		t.Errorf("invalid_grant: %v", err)
	}
	status, body = 500, `{"error_description":"upstream down"}`
	if err := chat(t, build()); err == nil || err.Error() != "Grok OAuth refresh failed: HTTP 500 upstream down" || !llm.IsTransient(err) {
		t.Errorf("500: %v", err)
	}
	status, body = 200, `{"refresh_token":"x"}`
	if err := chat(t, build()); err == nil || err.Error() != "Grok OAuth refresh succeeded but no access_token was returned." {
		t.Errorf("no token: %v", err)
	}
}

func TestGrokOAuthConcurrentCallsShareOneRefresh(t *testing.T) {
	var refreshes atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			refreshes.Add(1)
			time.Sleep(50 * time.Millisecond)
			io.WriteString(w, `{"access_token":"fresh","refresh_token":"r2","expires_in":3600}`)
			return
		}
		io.WriteString(w, chatOK)
	}))
	defer srv.Close()
	creds := oauth.New(oauth.Tokens{Access: "old", Refresh: "r1", ExpiresAtMs: time.Now().UnixMilli()}, nil)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		p := openaichat.NewGrokOAuth(openaichat.GrokOAuthConfig{Config: transport.Config{BaseURL: srv.URL + "/v1", HTTPClient: srv.Client()}, Credentials: creds, TokenURL: srv.URL + "/token"})
		wg.Add(1)
		go func() { defer wg.Done(); _ = chat(t, p) }()
	}
	wg.Wait()
	if refreshes.Load() != 1 {
		t.Errorf("refreshes = %d, want 1 (rotating refresh tokens are spent once)", refreshes.Load())
	}
}

func TestClaudeCodeRefreshesOn401(t *testing.T) {
	var auths []string
	var tokenBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/token":
			b, _ := io.ReadAll(r.Body)
			tokenBody = string(b)
			io.WriteString(w, `{"access_token":"new-access","refresh_token":"r2","expires_in":28800}`)
		case "/v1/messages":
			auths = append(auths, r.Header.Get("Authorization"))
			if r.Header.Get("Authorization") != "Bearer new-access" {
				w.WriteHeader(401)
				io.WriteString(w, `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`)
				return
			}
			io.WriteString(w, `{"model":"m","stop_reason":"end_turn","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":1,"output_tokens":1}}`)
		}
	}))
	defer srv.Close()
	creds := oauth.New(oauth.Tokens{Access: "stale", Refresh: "r1"}, nil)
	p := anthropic.NewClaudeCode(anthropic.ClaudeCodeConfig{
		Config:      transport.Config{BaseURL: srv.URL, HTTPClient: srv.Client()},
		Credentials: creds, TokenURL: srv.URL + "/oauth/token",
	})
	res, err := p.Chat(context.Background(), []llm.Message{{Role: "user", Content: "x"}}, nil, llm.Options{Model: "m"})
	if err != nil || res.Content != "hi" {
		t.Fatalf("chat = %+v, %v", res, err)
	}
	if strings.Join(auths, ",") != "Bearer stale,Bearer new-access" {
		t.Errorf("auths = %v", auths)
	}
	if tokenBody != `{"grant_type":"refresh_token","refresh_token":"r1","client_id":"9d1c250a-e61b-44d9-88ed-5944d1962f5e"}` {
		t.Errorf("refresh body = %s", tokenBody)
	}
}

func TestClaudeCodeScopeError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		io.WriteString(w, `{"type":"error","error":{"type":"permission_error","message":"OAuth token does not meet scope requirement user:inference"}}`)
	}))
	defer srv.Close()
	p := anthropic.NewClaudeCode(anthropic.ClaudeCodeConfig{Config: transport.Config{APIKey: "t", BaseURL: srv.URL, HTTPClient: srv.Client()}})
	_, err := p.Chat(context.Background(), nil, nil, llm.Options{Model: "m"})
	want := "Claude Code OAuth token is missing inference scope. Re-run `neoagent login claude-code` to create a token with " + anthropic.ClaudeCodeScopes + "."
	if err == nil || err.Error() != want {
		t.Errorf("err = %v", err)
	}
}

func TestCodexRefreshesOn401AndReadsAccountID(t *testing.T) {
	var auths, accounts []string
	var refreshBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			b, _ := io.ReadAll(r.Body)
			refreshBody = string(b)
			io.WriteString(w, `{"access_token":"acc2","refresh_token":"ref2"}`)
		case "/responses":
			auths = append(auths, r.Header.Get("Authorization"))
			accounts = append(accounts, r.Header.Get("ChatGPT-Account-Id"))
			if r.Header.Get("Authorization") != "Bearer acc2" {
				w.WriteHeader(401)
				io.WriteString(w, `{"error":{"message":"expired","type":"invalid_request_error","code":"token_expired"}}`)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"yo\"}\n\n"+
				"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n")
		}
	}))
	defer srv.Close()
	access := jwt(`{"https://api.openai.com/auth":{"chatgpt_account_id":"acct-42"}}`)
	creds := oauth.New(oauth.Tokens{Access: access, Refresh: "ref1"}, nil)
	p := codex.New(codex.Config{
		Config:      transport.Config{BaseURL: srv.URL, HTTPClient: srv.Client()},
		Credentials: creds, ForceBackend: true, TokenURL: srv.URL + "/token",
	})
	res, err := p.Chat(context.Background(), []llm.Message{{Role: "user", Content: "x"}}, nil, llm.Options{Model: "gpt-5-codex"})
	if err != nil || res.Content != "yo" {
		t.Fatalf("chat = %+v, %v", res, err)
	}
	if len(auths) != 2 || auths[1] != "Bearer acc2" || accounts[0] != "acct-42" {
		t.Errorf("auths %v accounts %v", auths, accounts)
	}
	if refreshBody != `{"client_id":"app_EMoamEEZ73f0CkXaXp7hrann","grant_type":"refresh_token","refresh_token":"ref1","scope":"openid profile email"}` {
		t.Errorf("refresh body = %s", refreshBody)
	}
}

func TestCodexErrorDetailsLikeNode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("x-request-id", "req_1")
		w.WriteHeader(400)
		io.WriteString(w, `{"error":{"message":"Unsupported parameter","type":"invalid_request_error","code":"unsupported","param":"temperature"}}`)
	}))
	defer srv.Close()
	p := codex.New(codex.Config{Config: transport.Config{APIKey: "t", BaseURL: srv.URL, HTTPClient: srv.Client()}})
	_, err := p.Chat(context.Background(), nil, nil, llm.Options{Model: "gpt-5"})
	want := "OpenAI Codex request failed: 400 Unsupported parameter (HTTP 400, type=invalid_request_error, code=unsupported, param=temperature, request_id=req_1)"
	if err == nil || err.Error() != want || llm.AsError(err).Status != 400 {
		t.Errorf("err = %v", err)
	}
}

func TestCodexBaseURLNormalisation(t *testing.T) {
	// the OpenAI API address and the ChatGPT backend both mean the Codex backend:
	// instructions are always present and the request is stored=false
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		w.WriteHeader(500)
	}))
	defer srv.Close()
	hc := &http.Client{Transport: roundTripperTo{srv}}
	for _, base := range []string{"", "https://api.openai.com/v1", "https://chatgpt.com/backend-api/codex/v1"} {
		p := codex.New(codex.Config{Config: transport.Config{APIKey: "t", BaseURL: base, HTTPClient: hc}})
		_, _ = p.Chat(context.Background(), []llm.Message{{Role: "user", Content: "x"}}, nil, llm.Options{Model: "gpt-5"})
		if !strings.Contains(body, `"store":false`) || !strings.Contains(body, `"instructions":""`) {
			t.Errorf("base %q not treated as the Codex backend: %s", base, body)
		}
	}
}

// roundTripperTo sends every request to the fake server whatever its address.
type roundTripperTo struct{ srv *httptest.Server }

func (r roundTripperTo) RoundTrip(req *http.Request) (*http.Response, error) {
	u, _ := url.Parse(r.srv.URL)
	req = req.Clone(req.Context())
	req.URL.Scheme, req.URL.Host = u.Scheme, u.Host
	return http.DefaultTransport.RoundTrip(req)
}

func TestGitHubCopilotExchangesTokenOnce(t *testing.T) {
	var exchanges atomic.Int32
	var chatAuth, editor string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/copilot/token":
			exchanges.Add(1)
			if r.Header.Get("Authorization") != "token gh-token" {
				t.Errorf("exchange auth = %q", r.Header.Get("Authorization"))
			}
			io.WriteString(w, `{"token":"cop-token","expires_at":`+itoa(time.Now().Unix()+1800)+`}`)
		case "/chat/completions":
			chatAuth, editor = r.Header.Get("Authorization"), r.Header.Get("Editor-Version")
			io.WriteString(w, chatOK)
		}
	}))
	defer srv.Close()
	p := openaichat.NewGitHubCopilot(openaichat.CopilotConfig{
		Config:   transport.Config{APIKey: "gh-token", BaseURL: srv.URL, HTTPClient: srv.Client()},
		TokenURL: srv.URL + "/copilot/token",
	})
	for i := 0; i < 2; i++ {
		if err := chat(t, p); err != nil {
			t.Fatal(err)
		}
	}
	if exchanges.Load() != 1 || chatAuth != "Bearer cop-token" || editor != "vscode/1.90.0" {
		t.Errorf("exchanges %d auth %q editor %q", exchanges.Load(), chatAuth, editor)
	}
	if !p.SupportsVision() {
		t.Error("copilot inherits OpenAI vision support")
	}
}

func TestGitHubCopilotTokenErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		io.WriteString(w, `Bad credentials token ghp_secretvalue`)
	}))
	defer srv.Close()
	p := openaichat.NewGitHubCopilot(openaichat.CopilotConfig{Config: transport.Config{APIKey: "gh", BaseURL: srv.URL, HTTPClient: srv.Client()}, TokenURL: srv.URL})
	err := chat(t, p)
	if err == nil || err.Error() != "Failed to refresh GitHub Copilot token: HTTP 401 - Bad credentials token [redacted]" {
		t.Errorf("err = %v", err)
	}
	p = openaichat.NewGitHubCopilot(openaichat.CopilotConfig{Config: transport.Config{BaseURL: srv.URL, HTTPClient: srv.Client()}})
	if err := chat(t, p); err == nil || err.Error() != "GitHub Copilot access token is missing. Please run `neoagent login github-copilot`." {
		t.Errorf("missing token: %v", err)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
