package httpx

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

var ok = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })

func do(h http.Handler, method, target string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestOriginsAllowed(t *testing.T) {
	empty := ParseOrigins("")
	cfg := ParseOrigins(" https://app.example.com , https://b.example.com ,")
	cases := []struct {
		o       Origins
		origin  string
		missing bool
		want    bool
	}{
		{empty, "http://localhost:3333", false, true},
		{empty, "http://127.0.0.1:5000", false, true},
		{empty, "http://[::1]:5000", false, true},
		{empty, "https://evil.example", false, false},
		{empty, "null", true, false},
		{empty, "", true, true},
		{empty, "", false, false},
		{cfg, "https://app.example.com", false, true},
		{cfg, "http://localhost:3333", false, false}, // configured list replaces the loopback default
		{cfg, "https://app.example.com.evil.io", false, false},
	}
	for _, c := range cases {
		if got := c.o.Allowed(c.origin, c.missing); got != c.want {
			t.Errorf("Allowed(%q, %v) = %v want %v", c.origin, c.missing, got, c.want)
		}
	}
}

func TestOriginGuardBlocksCrossSiteWrites(t *testing.T) {
	h := Chain(ok, OriginGuard(ParseOrigins("https://app.example.com")))
	if rec := do(h, "POST", "/api/x", map[string]string{"Origin": "https://evil.example"}); rec.Code != 403 {
		t.Fatalf("evil origin: %d", rec.Code)
	}
	if rec := do(h, "DELETE", "/api/x", map[string]string{"Referer": "https://evil.example/page"}); rec.Code != 403 {
		t.Fatalf("evil referer: %d", rec.Code)
	}
	if rec := do(h, "POST", "/api/x", map[string]string{"Origin": "https://app.example.com"}); rec.Code != 204 {
		t.Fatalf("good origin: %d", rec.Code)
	}
	if rec := do(h, "POST", "/api/x", nil); rec.Code != 204 {
		t.Fatalf("native client without origin: %d", rec.Code)
	}
	if rec := do(h, "GET", "/api/x", map[string]string{"Origin": "https://evil.example"}); rec.Code != 204 {
		t.Fatalf("safe methods are not guarded here: %d", rec.Code)
	}
	if rec := do(h, "POST", "/other", map[string]string{"Origin": "https://evil.example"}); rec.Code != 204 {
		t.Fatalf("non-api paths are not guarded: %d", rec.Code)
	}
}

func TestCORS(t *testing.T) {
	h := Chain(ok, CORS(ParseOrigins("https://app.example.com")))
	rec := do(h, "GET", "/api/x", map[string]string{"Origin": "https://app.example.com"})
	if rec.Header().Get("Access-Control-Allow-Origin") != "https://app.example.com" ||
		rec.Header().Get("Access-Control-Allow-Credentials") != "true" {
		t.Fatalf("headers: %v", rec.Header())
	}
	if rec := do(h, "GET", "/api/x", map[string]string{"Origin": "https://evil.example"}); rec.Code != 403 {
		t.Fatalf("evil: %d", rec.Code)
	}
	pre := do(h, "OPTIONS", "/api/x", map[string]string{
		"Origin": "https://app.example.com", "Access-Control-Request-Method": "POST", "Access-Control-Request-Headers": "content-type",
	})
	if pre.Code != 204 || pre.Header().Get("Access-Control-Allow-Headers") != "content-type" {
		t.Fatalf("preflight: %d %v", pre.Code, pre.Header())
	}
	if rec := do(h, "GET", "/api/x", nil); rec.Code != 204 || rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("requests without Origin get no CORS headers")
	}
}

func TestAPICaseGuardAndNoStore(t *testing.T) {
	h := Chain(ok, APICaseGuard(), NoStoreAPI())
	for _, p := range []string{"/API/auth", "/Api/x", "/aPI"} {
		if rec := do(h, "GET", p, nil); rec.Code != 404 || !strings.Contains(rec.Body.String(), "Not found") {
			t.Errorf("%s: %d", p, rec.Code)
		}
	}
	rec := do(h, "GET", "/api/health", nil)
	if rec.Code != 204 || rec.Header().Get("Cache-Control") != "no-store, max-age=0" || rec.Header().Get("Pragma") != "no-cache" {
		t.Fatalf("%d %v", rec.Code, rec.Header())
	}
	if do(h, "GET", "/apiary", nil).Code != 204 {
		t.Fatal("/apiary is not the api prefix")
	}
}

func TestRecoverHidesPanicDetail(t *testing.T) {
	boom := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { panic("secret internals") })
	h := Chain(boom, Recover(slog.New(slog.NewTextHandler(io.Discard, nil))))
	rec := do(h, "GET", "/api/x", nil)
	if rec.Code != 500 || strings.Contains(rec.Body.String(), "secret") {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestMaxBody(t *testing.T) {
	read := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.ReadAll(r.Body); err != nil {
			Error(w, http.StatusRequestEntityTooLarge, "too large")
			return
		}
		w.WriteHeader(204)
	})
	h := Chain(read, MaxBody(4))
	req := httptest.NewRequest("POST", "/", strings.NewReader("12345"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 413 {
		t.Fatalf("%d", rec.Code)
	}
}
