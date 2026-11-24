// Package secheaders sets the baseline browser-hardening response headers every
// NeoLabs app ships (the helmet defaults the Node servers used), plus an
// ordered Content-Security-Policy builder so each app declares its own policy.
package secheaders

import (
	"fmt"
	"net/http"
	"strings"
)

// Directive is one CSP directive. A Directive with no values renders as the
// bare name, which is how flags such as `upgrade-insecure-requests` are written.
type Directive struct {
	Name   string
	Values []string
}

// Policy is an ordered list of directives; order is kept so output is stable.
type Policy []Directive

// String renders the policy the way helmet does: `name v1 v2;name2 v`.
func (p Policy) String() string {
	parts := make([]string, 0, len(p))
	for _, d := range p {
		if len(d.Values) == 0 {
			parts = append(parts, d.Name)
			continue
		}
		parts = append(parts, d.Name+" "+strings.Join(d.Values, " "))
	}
	return strings.Join(parts, ";")
}

// Config selects the headers. Zero values leave optional headers off.
type Config struct {
	CSP               Policy
	HSTSMaxAgeSeconds int    // 0 disables Strict-Transport-Security
	ReferrerPolicy    string // default strict-origin-when-cross-origin
	CORP              string // Cross-Origin-Resource-Policy, default same-site
	PermissionsPolicy string // optional, e.g. "camera=(self), payment=()"
}

// Headers returns the headers for cfg.
func Headers(cfg Config) http.Header {
	h := http.Header{}
	set := func(k, v string) { h[http.CanonicalHeaderKey(k)] = []string{v} }
	if len(cfg.CSP) > 0 {
		set("Content-Security-Policy", cfg.CSP.String())
	}
	corp := cfg.CORP
	if corp == "" {
		corp = "same-site"
	}
	set("Cross-Origin-Resource-Policy", corp)
	referrer := cfg.ReferrerPolicy
	if referrer == "" {
		referrer = "strict-origin-when-cross-origin"
	}
	set("Referrer-Policy", referrer)
	if cfg.HSTSMaxAgeSeconds > 0 {
		set("Strict-Transport-Security", fmt.Sprintf("max-age=%d", cfg.HSTSMaxAgeSeconds))
	}
	set("X-Content-Type-Options", "nosniff")
	set("X-DNS-Prefetch-Control", "off")
	set("X-Download-Options", "noopen")
	set("X-Frame-Options", "SAMEORIGIN")
	set("X-Permitted-Cross-Domain-Policies", "none")
	set("X-XSS-Protection", "0")
	if cfg.PermissionsPolicy != "" {
		set("Permissions-Policy", cfg.PermissionsPolicy)
	}
	return h
}

// Middleware applies the headers to every response.
func Middleware(cfg Config) func(http.Handler) http.Handler {
	headers := Headers(cfg)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			dst := w.Header()
			for k, v := range headers {
				dst[k] = v
			}
			next.ServeHTTP(w, r)
		})
	}
}
