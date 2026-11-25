package transport

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/NeoLabs-Systems/neolabs-go/llm"
	"github.com/NeoLabs-Systems/neolabs-go/ssrf"
)

// Config is what every provider client takes.
type Config struct {
	APIKey  string
	BaseURL string
	// HTTPClient replaces the default client (tests, a corporate proxy).
	HTTPClient *http.Client
	// TrustedBaseURL says the base URL is the provider's own default or the
	// operator's, so private addresses are allowed (Ollama on localhost, a
	// NeoRouter on the home network). Otherwise the default client refuses
	// loopback, private and link-local destinations through neolabs-go/ssrf: a
	// user-supplied base URL must never reach the server's internal network.
	TrustedBaseURL bool
	// Headers are sent with every request.
	Headers map[string]string
}

// streamCeiling bounds one whole request including its streamed body; the
// caller's context ends requests sooner.
const streamCeiling = time.Hour

// Client returns the HTTP client for the configuration.
func (c Config) Client() *http.Client {
	switch {
	case c.HTTPClient != nil:
		return c.HTTPClient
	case c.TrustedBaseURL:
		return &http.Client{Transport: http.DefaultTransport}
	}
	return publicOnlyClient()
}

// publicOnlyClient is shared by every provider instance: runs build a fresh
// provider each time, and a client per instance would pile up idle connections.
var publicOnlyClient = sync.OnceValue(func() *http.Client {
	return ssrf.NewClient(ssrf.Options{Timeout: streamCeiling})
})

// Request is one HTTP call.
type Request struct {
	Method string
	URL    string
	Header map[string]string
	Body   []byte
}

// Do sends the request. A cancelled context returns its own error; a refused
// destination is a plain error; anything else is a retryable connection error.
func Do(ctx context.Context, hc *http.Client, provider, connectionMessage string, r Request) (*http.Response, error) {
	method := r.Method
	if method == "" {
		method = http.MethodGet
	}
	var body io.Reader
	if r.Body != nil {
		body = bytes.NewReader(r.Body)
	}
	req, err := http.NewRequestWithContext(ctx, method, r.URL, body)
	if err != nil {
		return nil, &llm.Error{Provider: provider, Message: err.Error()}
	}
	for k, v := range r.Header {
		req.Header.Set(k, v)
	}
	resp, err := hc.Do(req)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		if errors.Is(err, ssrf.ErrBlocked) {
			return nil, &llm.Error{Provider: provider, Message: err.Error()}
		}
		return nil, &llm.Error{Provider: provider, Message: connectionMessage, Retryable: true}
	}
	return resp, nil
}

// JoinURL appends path to base like the vendor SDKs do.
func JoinURL(base, path string) string {
	if strings.HasSuffix(base, "/") && strings.HasPrefix(path, "/") {
		return base + path[1:]
	}
	return base + path
}

// CheckStatus turns a non-2xx response into an llm.Error and closes the body.
// bodyIsError is true for APIs whose whole JSON body is the error (Anthropic).
func CheckStatus(provider string, resp *http.Response, bodyIsError bool) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return llm.StatusError(provider, resp.StatusCode, resp.Header, body, bodyIsError)
}

// ReadBody reads a body up to max bytes; longer bodies fail like Node's
// readResponseBuffer.
func ReadBody(r io.Reader, max int64, service string) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > max {
		return nil, &llm.Error{Code: llm.CodeResponseTooLarge, Message: fmt.Sprintf("%s response exceeded the %d-byte safety limit.", service, max)}
	}
	return body, nil
}

// FetchOptions drive FetchText.
type FetchOptions struct {
	Method   string
	URL      string
	Header   map[string]string
	Body     []byte
	MaxBytes int64
	Service  string
	Timeout  time.Duration // default 15s
	Provider string
}

// TextResponse is a fully read response.
type TextResponse struct {
	Status int
	Header http.Header
	Text   string
}

// OK reports a 2xx status.
func (t *TextResponse) OK() bool { return t.Status >= 200 && t.Status < 300 }

// FetchText is fetchResponseText: one request with a deadline and a response
// size limit. A deadline reads "<service> request timed out after <n>ms.".
func FetchText(ctx context.Context, hc *http.Client, o FetchOptions) (*TextResponse, error) {
	timeout := o.Timeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	max := o.MaxBytes
	if max <= 0 {
		max = 10 << 20
	}
	service := strings.TrimSpace(o.Service)
	if service == "" {
		service = "Remote service"
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	fail := func(err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if callCtx.Err() != nil {
			return &llm.Error{Provider: o.Provider, Code: llm.CodeHTTPTimeout, Message: fmt.Sprintf("%s request timed out after %dms.", service, timeout.Milliseconds())}
		}
		return err
	}
	resp, err := Do(callCtx, hc, o.Provider, "fetch failed", Request{Method: o.Method, URL: o.URL, Header: o.Header, Body: o.Body})
	if err != nil {
		return nil, fail(err)
	}
	defer resp.Body.Close()
	body, err := ReadBody(resp.Body, max, service)
	if err != nil {
		return nil, fail(err)
	}
	return &TextResponse{Status: resp.StatusCode, Header: resp.Header, Text: string(body)}, nil
}

// Flight shares one in-flight call per key: concurrent callers wait for the
// same result (refresh tokens rotate, so each must be spent only once).
type Flight[T any] struct {
	mu    sync.Mutex
	calls map[string]*flightCall[T]
}

type flightCall[T any] struct {
	done  chan struct{}
	value T
	err   error
}

// Do runs fn for key unless a call for key is already running; callers stop
// waiting when ctx ends.
func (f *Flight[T]) Do(ctx context.Context, key string, fn func() (T, error)) (T, error) {
	f.mu.Lock()
	if f.calls == nil {
		f.calls = map[string]*flightCall[T]{}
	}
	call, running := f.calls[key]
	if !running {
		call = &flightCall[T]{done: make(chan struct{})}
		f.calls[key] = call
		go func() {
			call.value, call.err = fn()
			f.mu.Lock()
			delete(f.calls, key)
			f.mu.Unlock()
			close(call.done)
		}()
	}
	f.mu.Unlock()
	select {
	case <-call.done:
		return call.value, call.err
	case <-ctx.Done():
		var zero T
		return zero, ctx.Err()
	}
}
