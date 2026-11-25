// Package oauthcli signs a command-line program in to an OAuth provider: the
// browser flow with PKCE and a loopback redirect (RFC 8252), and the device
// authorization flow (RFC 8628) where the person types a code on the
// provider's site.
package oauthcli

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// MaxResponseBytes bounds every token endpoint response.
const MaxResponseBytes = 256 << 10

// PKCE is a verifier and its S256 challenge.
type PKCE struct{ Verifier, Challenge string }

// NewPKCE returns a fresh verifier (48 random bytes) and challenge.
func NewPKCE() PKCE {
	b := make([]byte, 48)
	_, _ = rand.Read(b)
	v := base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(v))
	return PKCE{Verifier: v, Challenge: base64.RawURLEncoding.EncodeToString(sum[:])}
}

// NewState returns a random hex state value.
func NewState() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

const (
	pageOK     = "<html><body><h2>Authorization successful!</h2><p>You can close this tab and return to the terminal.</p></body></html>"
	pageFailed = "<html><body><h2>Authorization failed.</h2><p>You can close this tab.</p></body></html>"
	pageState  = "<html><body><h2>Authorization failed.</h2><p>State mismatch. You can close this tab.</p></body></html>"
	pageNoCode = "<html><body><h2>Authorization failed.</h2><p>Missing authorization code.</p></body></html>"
)

// Loopback receives the authorization code on a local redirect URI.
type Loopback struct {
	listener net.Listener
	// RedirectURI is the URI to register in the authorization request.
	RedirectURI string
	path, state string
}

// ListenLoopback listens on host:port (port 0 picks one) for the redirect.
func ListenLoopback(host string, port int, path, state string) (*Loopback, error) {
	l, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return nil, fmt.Errorf("Could not start OAuth callback server on port %d: %w", port, err)
	}
	actual := l.Addr().(*net.TCPAddr).Port
	return &Loopback{listener: l, RedirectURI: fmt.Sprintf("http://%s:%d%s", host, actual, path), path: path, state: state}, nil
}

// Wait serves the redirect until a code (or an error) arrives or timeout.
func (l *Loopback) Wait(ctx context.Context, timeout time.Duration) (string, error) {
	type result struct {
		code string
		err  error
	}
	done := make(chan result, 1)
	finish := func(r result) {
		select {
		case done <- r:
		default:
		}
	}
	srv := &http.Server{ReadHeaderTimeout: 10 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		q := r.URL.Query()
		switch {
		case q.Get("error") != "":
			_, _ = io.WriteString(w, pageFailed)
			finish(result{err: fmt.Errorf("OAuth error: %s", q.Get("error"))})
		case q.Get("state") != l.state:
			_, _ = io.WriteString(w, pageState)
			finish(result{err: errors.New("OAuth state mismatch — possible CSRF attempt.")})
		case q.Get("code") != "":
			_, _ = io.WriteString(w, pageOK)
			finish(result{code: q.Get("code")})
		default:
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, pageNoCode)
		}
	})}
	go func() { _ = srv.Serve(l.listener) }()
	defer srv.Close()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case r := <-done:
		return r.code, r.err
	case <-timer.C:
		return "", fmt.Errorf("Authorization timed out after %d minutes.", int(timeout.Minutes()))
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// Close stops listening.
func (l *Loopback) Close() { l.listener.Close() }

// PostJSON posts body (a JSON value, or url.Values as a form) and decodes a
// bounded JSON object; any non-2xx status is an error carrying the response.
func PostJSON(ctx context.Context, client *http.Client, endpoint string, headers map[string]string, body any) (map[string]any, int, error) {
	var reader io.Reader
	contentType := "application/json"
	if form, ok := body.(url.Values); ok {
		reader = strings.NewReader(form.Encode())
		contentType = "application/x-www-form-urlencoded"
	} else {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, reader)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Accept", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBytes+1))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	if len(raw) > MaxResponseBytes {
		return nil, resp.StatusCode, errors.New("the authorization response exceeded its size limit")
	}
	var data map[string]any
	_ = json.Unmarshal(raw, &data)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		text := string(raw)
		if len(text) > 500 {
			text = text[:500]
		}
		return data, resp.StatusCode, fmt.Errorf("HTTP %d — %s", resp.StatusCode, text)
	}
	if data == nil {
		return nil, resp.StatusCode, errors.New("the authorization response was not a JSON object")
	}
	return data, resp.StatusCode, nil
}

// Str reads a string field of a decoded response.
func Str(data map[string]any, key string) string {
	if v, ok := data[key].(string); ok {
		return v
	}
	return ""
}

// PollDevice polls a device-authorization token endpoint until done reports
// success, honouring authorization_pending and slow_down; 403/404 answers
// and network errors are retried until timeout.
func PollDevice(ctx context.Context, client *http.Client, endpoint string, body func() any, interval, timeout time.Duration, done func(map[string]any) bool) error {
	if interval < time.Second {
		interval = 5 * time.Second
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
		data, status, err := PostJSON(ctx, client, endpoint, nil, body())
		if status == http.StatusForbidden || status == http.StatusNotFound {
			continue
		}
		if err != nil {
			if status == 0 {
				continue // network trouble: try again
			}
			return fmt.Errorf("Token poll failed: %w", err)
		}
		if done(data) {
			return nil
		}
		switch Str(data, "error") {
		case "", "authorization_pending":
			continue
		case "slow_down":
			interval = min(interval+5*time.Second, 30*time.Second)
			continue
		}
		msg := Str(data, "error_description")
		if msg == "" {
			msg = Str(data, "error")
		}
		return fmt.Errorf("Authentication failed: %s", msg)
	}
	return fmt.Errorf("Authentication timed out after %d minutes.", int((timeout+time.Minute-1)/time.Minute))
}
