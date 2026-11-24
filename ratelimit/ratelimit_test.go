package ratelimit

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The headers and body mirror a capture of express-rate-limit 8.6.2 with
// {windowMs: 15min, max: 2, standardHeaders: true, legacyHeaders: false}.
func TestMatchesExpressRateLimit(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	l := New(Config{
		Window:  15 * time.Minute,
		Max:     2,
		Message: map[string]string{"error": "Too many attempts, try again later"},
		Now:     func() time.Time { return now },
	})
	h := l.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"ok":1}`)) }))

	type want struct {
		status    int
		remaining string
		retry     string
	}
	for i, w := range []want{{200, "1", ""}, {200, "0", ""}, {429, "0", "900"}} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/", nil)
		req.RemoteAddr = "203.0.113.9:5555"
		h.ServeHTTP(rec, req)
		if rec.Code != w.status {
			t.Fatalf("request %d: status %d", i, rec.Code)
		}
		hd := rec.Header()
		if hd.Get("RateLimit-Limit") != "2" || hd.Get("RateLimit-Policy") != "2;w=900" ||
			hd.Get("RateLimit-Remaining") != w.remaining || hd.Get("RateLimit-Reset") != "900" ||
			hd.Get("Retry-After") != w.retry {
			t.Fatalf("request %d headers: %v", i, hd)
		}
		if i == 2 && strings.TrimSpace(rec.Body.String()) != `{"error":"Too many attempts, try again later"}` {
			t.Fatalf("429 body: %q", rec.Body.String())
		}
	}
}

func TestPerClientAndWindowReset(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	l := New(Config{Window: time.Minute, Max: 1, Now: func() time.Time { return now }})
	h := l.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	call := func(addr string) int {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/", nil)
		req.RemoteAddr = addr
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	if call("1.1.1.1:1") != 200 || call("1.1.1.1:2") != 429 {
		t.Fatal("second hit from the same IP must be limited")
	}
	if call("2.2.2.2:1") != 200 {
		t.Fatal("another client has its own budget")
	}
	now = now.Add(61 * time.Second)
	if call("1.1.1.1:3") != 200 {
		t.Fatal("window did not reset")
	}
}
