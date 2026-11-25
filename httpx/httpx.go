// Package httpx is the shared HTTP toolkit of the NeoLabs apps: JSON helpers,
// middleware composition and the browser-origin protections (CORS, CSRF-style
// origin guard) that every app's `/api` surface relies on.
package httpx

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
)

// Middleware wraps a handler.
type Middleware func(http.Handler) http.Handler

// Chain applies middleware so that the first one is the outermost.
func Chain(h http.Handler, mws ...Middleware) http.Handler {
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
	}
	return h
}

// WriteJSON writes v as JSON with the given status.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	// Express writes JSON without HTML escaping and without a trailing newline;
	// stay identical so message text and byte counts match the Node servers.
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	body := bytes.TrimRight(buf.Bytes(), "\n")
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// Error writes the `{"error": message}` envelope the Flutter clients parse.
func Error(w http.ResponseWriter, status int, message string) {
	WriteJSON(w, status, map[string]string{"error": message})
}

// ErrorCode writes `{"error": message, "code": code}`.
func ErrorCode(w http.ResponseWriter, status int, message, code string) {
	WriteJSON(w, status, map[string]string{"error": message, "code": code})
}

// Recover turns a panic into a generic 500 without leaking the cause, and logs
// the stack for operators.
func Recover(log *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					if rec == http.ErrAbortHandler {
						panic(rec)
					}
					log.Error("panic in handler", "method", r.Method, "path", r.URL.Path, "panic", rec, "stack", string(debug.Stack()))
					Error(w, http.StatusInternalServerError, "Internal server error")
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// MaxBody caps request bodies; reads past the limit fail with an error that
// handlers map to 413.
func MaxBody(n int64) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.Body = http.MaxBytesReader(w, r.Body, n)
			next.ServeHTTP(w, r)
		})
	}
}

var apiPrefix = regexp.MustCompile(`(?i)^/api(/|$)`)

// APICaseGuard rejects `/API/...` style paths. Go's router is case sensitive,
// but a proxy in front may not be, and the origin check and error redaction
// look for the lowercase prefix, so mixed case must never reach a handler.
func APICaseGuard() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p := r.URL.Path
			if apiPrefix.MatchString(p) && !strings.HasPrefix(p, "/api/") && p != "/api" {
				Error(w, http.StatusNotFound, "Not found")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// NoStoreAPI marks every `/api/` response uncacheable.
func NoStoreAPI() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/api/") {
				h := w.Header()
				h.Set("Cache-Control", "no-store, max-age=0")
				h.Set("Pragma", "no-cache")
				h.Set("Expires", "0")
			}
			next.ServeHTTP(w, r)
		})
	}
}

// Origins is the set of browser origins allowed to call the API with
// credentials. With no configured origins only loopback origins are allowed,
// which is the safe default for a self-hosted install.
type Origins struct {
	configured []string
}

// ParseOrigins reads a comma separated list such as the ALLOWED_ORIGINS value.
func ParseOrigins(list string) Origins {
	var o Origins
	for _, part := range strings.Split(list, ",") {
		if part = strings.TrimSpace(part); part != "" {
			o.configured = append(o.configured, part)
		}
	}
	return o
}

func isLoopback(origin string) bool {
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	switch u.Hostname() {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}

// Allowed reports whether origin may use the API. An empty origin (non-browser
// clients, same-origin navigations) is allowed only when allowMissing is true.
func (o Origins) Allowed(origin string, allowMissing bool) bool {
	if origin == "" {
		return allowMissing
	}
	if origin == "null" {
		return false
	}
	for _, c := range o.configured {
		if c == origin {
			return true
		}
	}
	return len(o.configured) == 0 && isLoopback(origin)
}

const corsMethods = "GET,HEAD,PUT,PATCH,POST,DELETE"

// CORS answers cross-origin requests for allowed origins with credentials
// support and rejects other browser origins with 403.
func CORS(o Origins) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			if origin == "" {
				next.ServeHTTP(w, r)
				return
			}
			if !o.Allowed(origin, false) {
				Error(w, http.StatusForbidden, "Origin not allowed")
				return
			}
			h := w.Header()
			h.Set("Access-Control-Allow-Origin", origin)
			h.Set("Access-Control-Allow-Credentials", "true")
			h.Add("Vary", "Origin")
			if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
				h.Set("Access-Control-Allow-Methods", corsMethods)
				if reqHeaders := r.Header.Get("Access-Control-Request-Headers"); reqHeaders != "" {
					h.Set("Access-Control-Allow-Headers", reqHeaders)
					h.Add("Vary", "Access-Control-Request-Headers")
				}
				h.Set("Content-Length", "0")
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func originOfReferer(referer string) string {
	u, err := url.Parse(strings.TrimSpace(referer))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

func isStateChanging(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	}
	return false
}

// OriginGuard blocks state-changing `/api/` requests whose Origin (or Referer)
// is not allowed. Requests without either header are native clients and pass.
func OriginGuard(o Origins) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/api/") && isStateChanging(r.Method) {
				origin := strings.TrimSpace(r.Header.Get("Origin"))
				if origin == "" {
					origin = originOfReferer(r.Header.Get("Referer"))
				}
				if origin != "" && !o.Allowed(origin, false) {
					Error(w, http.StatusForbidden, "Origin not allowed")
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

// ClientIP is the address of the client. Behind a trusted proxy it is the last
// X-Forwarded-For entry, the one the proxy itself appended; earlier entries are
// supplied by the client and can be forged, so they are never used.
func ClientIP(r *http.Request, trustProxy bool) string {
	if trustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			if ip := strings.TrimSpace(parts[len(parts)-1]); ip != "" {
				return strings.TrimPrefix(ip, "::ffff:")
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	return strings.TrimPrefix(host, "::ffff:")
}
