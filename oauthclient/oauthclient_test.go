package oauthclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func newClient() *Client {
	return &Client{HTTP: http.DefaultClient, Service: "Test", Sleep: func(context.Context, time.Duration) error { return nil }}
}

func TestAppendQueryMatchesURLSearchParams(t *testing.T) {
	// Recorded from Node: appendQuery('https://x.test/a?b=1', {...}).
	got, err := AppendQuery("https://x.test/a?b=1",
		"client_id", "a b", "redirect_uri", "https://h/cb?x=~*!()", "scope", "a b  c", "blank", " ", "state", "é&=")
	if err != nil {
		t.Fatal(err)
	}
	want := "https://x.test/a?b=1&client_id=a+b&redirect_uri=https%3A%2F%2Fh%2Fcb%3Fx%3D%7E*%21%28%29&scope=a+b++c&state=%C3%A9%26%3D"
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

func TestJSONErrors(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"description wins", 400, `{"error":"invalid_grant","error_description":"Bad code"}`, "Test request failed: Bad code"},
		{"error string", 401, `{"error":"nope"}`, "Test request failed: nope"},
		{"error object", 400, `{"error":{"message":"deep"}}`, "Test request failed: [object Object]"},
		{"message", 404, `{"message":"missing"}`, "Test request failed: missing"},
		{"plain text", 500, `boom`, "Test request failed: boom"},
		{"empty body", 502, ``, "Test request failed: 502 Bad Gateway"},
		{"slack envelope", 200, `{"ok":false,"error":"channel_not_found"}`, "Test request failed: channel_not_found"},
		{"token redacted", 400, `{"message":"bad Bearer abc.def and access_token=zzz"}`, "Test request failed: bad Bearer [redacted] and access_token=[redacted]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			_, err := newClient().JSON(context.Background(), Request{URL: srv.URL, Method: "POST"})
			var pe *Error
			if !errors.As(err, &pe) || pe.Message != tc.want || pe.Status != tc.status {
				t.Fatalf("got %v (%T), want %q", err, err, tc.want)
			}
		})
	}
}

func TestJSONSuccessShapes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/obj":
			w.Write([]byte(` {"z":1,"a":[1,2]} `))
		case "/html":
			w.Write([]byte(`<html>`))
		case "/echo":
			b := make([]byte, 100)
			n, _ := r.Body.Read(b)
			enc := json.NewEncoder(w)
			enc.SetEscapeHTML(false)
			enc.Encode(r.Header.Get("Content-Type") + "|" + string(b[:n]))
		}
	}))
	defer srv.Close()
	c := newClient()
	raw, err := c.JSON(context.Background(), Request{URL: srv.URL + "/obj"})
	if err != nil || string(raw) != `{"z":1,"a":[1,2]}` {
		t.Fatalf("obj: %s %v", raw, err)
	}
	raw, err = c.JSON(context.Background(), Request{URL: srv.URL + "/html"})
	if err != nil || string(raw) != "null" {
		t.Fatalf("html: %s %v", raw, err)
	}
	raw, _ = c.JSON(context.Background(), Request{URL: srv.URL + "/echo", Method: "POST", JSON: map[string]any{"t": "<a>"}})
	if string(raw) != `"application/json|{\"t\":\"<a>\"}"` {
		t.Fatalf("json body not sent unescaped: %s", raw)
	}
	raw, _ = c.JSON(context.Background(), Request{URL: srv.URL + "/echo", Method: "POST", Form: [][2]string{{"a", "b c"}, {"d", "é"}}})
	if !strings.Contains(string(raw), "application/x-www-form-urlencoded") || !strings.Contains(string(raw), "a=b+c&d=%C3%A9") {
		t.Fatalf("form: %s", raw)
	}
}

func TestRetryOnlyIdempotentAndTransient(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(503)
			return
		}
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	if _, err := newClient().JSON(context.Background(), Request{URL: srv.URL}); err != nil || calls.Load() != 2 {
		t.Fatalf("GET should retry once: %v calls=%d", err, calls.Load())
	}
	calls.Store(0)
	if _, err := newClient().JSON(context.Background(), Request{URL: srv.URL, Method: "POST"}); err == nil || calls.Load() != 1 {
		t.Fatalf("POST must not retry: %v calls=%d", err, calls.Load())
	}
	calls.Store(0)
	if _, err := newClient().JSON(context.Background(), Request{URL: srv.URL, NoRetry: true}); err == nil || calls.Load() != 1 {
		t.Fatalf("NoRetry must not retry: %v calls=%d", err, calls.Load())
	}
}

func TestRetryRateLimitedAndLongWait(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(403)
	}))
	defer srv.Close()
	if _, err := newClient().JSON(context.Background(), Request{URL: srv.URL}); err == nil || calls.Load() != 1 {
		t.Fatalf("a wait over the cap fails without retrying: calls=%d", calls.Load())
	}
	// Slack answers 200 {"ok":false,"error":"ratelimited"}.
	calls.Store(0)
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Write([]byte(`{"ok":false,"error":"ratelimited"}`))
			return
		}
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv2.Close()
	if _, err := newClient().JSON(context.Background(), Request{URL: srv2.URL}); err != nil || calls.Load() != 2 {
		t.Fatalf("ratelimited should retry: %v calls=%d", err, calls.Load())
	}
}

func TestLimitsAndTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow" {
			time.Sleep(300 * time.Millisecond)
		}
		w.Write([]byte(strings.Repeat("x", 100)))
	}))
	defer srv.Close()
	_, err := newClient().Do(context.Background(), Request{URL: srv.URL, MaxBytes: 10})
	if err == nil || err.Error() != "Test response exceeded the 10-byte safety limit." {
		t.Fatalf("size: %v", err)
	}
	_, err = newClient().Do(context.Background(), Request{URL: srv.URL + "/slow", Timeout: 50 * time.Millisecond})
	if err == nil || err.Error() != "Test request timed out after 50ms." {
		t.Fatalf("timeout: %v", err)
	}
}

func TestHelpers(t *testing.T) {
	// RFC 7636 appendix B.
	if got := CodeChallenge("dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"); got != "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM" {
		t.Fatalf("challenge %s", got)
	}
	now := time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)
	if got := ExpiresAt(float64(60), false, now); got != "2026-11-24T10:01:00.000Z" {
		t.Fatal(got)
	}
	if got := ExpiresAt(nil, false, now); got != "2026-11-24T11:00:00.000Z" {
		t.Fatal(got)
	}
	if got := ExpiresAt("x", true, now); got != nil {
		t.Fatal(got)
	}
	if got := Merge(Credentials{"refresh_token": "r", "a": 1}, Credentials{"access_token": "n", "refresh_token": ""}); got["refresh_token"] != "r" || got["access_token"] != "n" {
		t.Fatal(got)
	}
	if d, ok := RetryAfter(http.Header{"Retry-After": {"2"}}, now); !ok || d != 2*time.Second {
		t.Fatal(d, ok)
	}
	if SanitizeRemote("  "+strings.Repeat("a", 1200)) == "" || len(SanitizeRemote(strings.Repeat("a", 1200))) != 1000 {
		t.Fatal("slice")
	}
}

func TestWithRefresh(t *testing.T) {
	refreshes := 0
	refresh := func(_ context.Context, c Credentials) (Credentials, error) {
		refreshes++
		n := c.Clone()
		n["access_token"] = "fresh"
		n["expires_at"] = ISO(time.Now().Add(time.Hour))
		return n, nil
	}
	expired := Credentials{"access_token": "old", "refresh_token": "r", "expires_at": ISO(time.Now().Add(-time.Hour))}
	got, creds, err := WithRefresh(context.Background(), expired, refresh, func(c Credentials) (string, error) { return c.String("access_token"), nil }, nil)
	if err != nil || got != "fresh" || refreshes != 1 || creds.String("access_token") != "fresh" {
		t.Fatal(got, refreshes, err)
	}
	// A 401 with a refresh token refreshes once and retries.
	refreshes = 0
	live := Credentials{"access_token": "old", "refresh_token": "r"}
	got, _, err = WithRefresh(context.Background(), live, refresh, func(c Credentials) (string, error) {
		if c.String("access_token") == "old" {
			return "", &Error{Status: 401, Message: "no"}
		}
		return c.String("access_token"), nil
	}, nil)
	if err != nil || got != "fresh" || refreshes != 1 {
		t.Fatal(got, refreshes, err)
	}
	// Without a refresh token the 401 is final.
	_, _, err = WithRefresh(context.Background(), Credentials{"access_token": "old"}, refresh, func(Credentials) (string, error) { return "", &Error{Status: 401} }, nil)
	if err == nil {
		t.Fatal("expected error")
	}
}
