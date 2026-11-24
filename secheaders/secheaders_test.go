package secheaders

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPolicyRendering(t *testing.T) {
	p := Policy{
		{"default-src", []string{"'self'"}},
		{"script-src", []string{"'self'", "blob:"}},
		{"upgrade-insecure-requests", nil},
	}
	want := "default-src 'self';script-src 'self' blob:;upgrade-insecure-requests"
	if got := p.String(); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestBaselineHeaders(t *testing.T) {
	h := Headers(Config{HSTSMaxAgeSeconds: 86400, PermissionsPolicy: "camera=(self), payment=()"})
	want := map[string]string{
		"Cross-Origin-Resource-Policy":      "same-site",
		"Referrer-Policy":                   "strict-origin-when-cross-origin",
		"Strict-Transport-Security":         "max-age=86400",
		"X-Content-Type-Options":            "nosniff",
		"X-DNS-Prefetch-Control":            "off",
		"X-Download-Options":                "noopen",
		"X-Frame-Options":                   "SAMEORIGIN",
		"X-Permitted-Cross-Domain-Policies": "none",
		"X-XSS-Protection":                  "0",
		"Permissions-Policy":                "camera=(self), payment=()",
	}
	for k, v := range want {
		if got := h.Get(k); got != v {
			t.Errorf("%s: got %q want %q", k, got, v)
		}
	}
	if Headers(Config{}).Get("Strict-Transport-Security") != "" {
		t.Error("HSTS must be off by default (plain http deployments)")
	}
	if Headers(Config{}).Get("Content-Security-Policy") != "" {
		t.Error("no CSP without a policy")
	}
}

func TestMiddlewareAppliesToEveryResponse(t *testing.T) {
	h := Middleware(Config{CSP: Policy{{"default-src", []string{"'self'"}}}})(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(404) }))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/x", nil))
	if rec.Header().Get("Content-Security-Policy") != "default-src 'self'" || rec.Header().Get("X-Frame-Options") != "SAMEORIGIN" {
		t.Fatalf("headers missing: %v", rec.Header())
	}
}
