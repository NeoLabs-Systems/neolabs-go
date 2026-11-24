package sessionx

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Options configure a Manager. The defaults mirror the Node server: HttpOnly,
// SameSite=Lax, 30 day rolling lifetime, signed cookie.
type Options struct {
	Name       string
	Secrets    []string // first signs; the rest only verify (rotation)
	MaxAge     time.Duration
	Secure     bool // cookie is only sent over HTTPS
	TrustProxy bool // honour X-Forwarded-Proto when deciding if a request is secure
	SameSite   http.SameSite
	Rolling    bool // refresh expiry on every response
	Path       string
}

// Manager loads sessions from a Store and writes them back after the handler runs.
type Manager struct {
	store *Store
	opt   Options
}

// NewManager validates options and returns a Manager.
func NewManager(store *Store, opt Options) (*Manager, error) {
	if store == nil || opt.Name == "" || len(opt.Secrets) == 0 || opt.Secrets[0] == "" {
		return nil, errors.New("sessionx: store, cookie name and a secret are required")
	}
	if opt.MaxAge <= 0 {
		opt.MaxAge = 30 * 24 * time.Hour
	}
	if opt.SameSite == 0 {
		opt.SameSite = http.SameSiteLaxMode
	}
	if opt.Path == "" {
		opt.Path = "/"
	}
	return &Manager{store: store, opt: opt}, nil
}

// Session is one request's view of the stored session. It is safe for
// concurrent use by handlers of the same request.
type Session struct {
	mu        sync.Mutex
	id        string
	data      map[string]any
	isNew     bool
	modified  bool
	destroyed bool
	oldID     string // set by Regenerate so the old row is removed
}

type ctxKey struct{}

// FromContext returns the request's session, or nil outside the middleware.
func FromContext(ctx context.Context) *Session {
	s, _ := ctx.Value(ctxKey{}).(*Session)
	return s
}

// Get returns a value stored under key.
func (s *Session) Get(key string) (any, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.data[key]
	return v, ok
}

// String returns a value as text, including numbers stored as json.Number.
func (s *Session) String(key string) string {
	v, ok := s.Get(key)
	if !ok || v == nil {
		return ""
	}
	switch t := v.(type) {
	case string:
		return t
	case interface{ String() string }:
		return t.String()
	}
	return ""
}

// Set stores a value; it is persisted when the response is written.
func (s *Session) Set(key string, value any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[key] = value
	s.modified = true
}

// Delete removes one key.
func (s *Session) Delete(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.data[key]; ok {
		delete(s.data, key)
		s.modified = true
	}
}

// Destroy ends the session; the stored row is removed and no cookie is refreshed.
func (s *Session) Destroy() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.destroyed = true
	s.data = map[string]any{}
}

// Regenerate issues a fresh session id and keeps the data. Call it right after
// authentication so a pre-login id can never be reused (session fixation).
func (s *Session) Regenerate() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.isNew {
		s.oldID = s.id
	}
	s.id = newID()
	s.isNew = true
	s.modified = true
}

func newID() string {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		panic("sessionx: crypto/rand failed: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(buf)
}

func (m *Manager) isSecureRequest(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	if m.opt.TrustProxy {
		proto := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Proto"), ",")[0])
		return strings.EqualFold(proto, "https")
	}
	return false
}

// Middleware attaches the session to the request context.
func (m *Manager) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sess := &Session{data: map[string]any{}}
		if c, err := r.Cookie(m.opt.Name); err == nil {
			if sid, ok := DecodeCookieValue(c.Value, m.opt.Secrets...); ok {
				if data, err := m.store.Get(r.Context(), sid); err == nil && data != nil {
					sess.id, sess.data = sid, data
					delete(sess.data, "cookie")
				}
			}
		}
		if sess.id == "" {
			sess.id, sess.isNew = newID(), true
		}
		cw := &commitWriter{ResponseWriter: w, m: m, r: r, sess: sess}
		next.ServeHTTP(cw, r.WithContext(context.WithValue(r.Context(), ctxKey{}, sess)))
		cw.commit() // handlers that wrote nothing still persist changes
	})
}

type commitWriter struct {
	http.ResponseWriter
	m    *Manager
	r    *http.Request
	sess *Session
	done bool
}

// commit persists the session and sets the cookie, once, before headers go out.
func (c *commitWriter) commit() {
	if c.done {
		return
	}
	c.done = true
	s, m := c.sess, c.m
	s.mu.Lock()
	defer s.mu.Unlock()
	ctx := context.WithoutCancel(c.r.Context())

	if s.destroyed {
		_ = m.store.Destroy(ctx, s.id)
		return
	}
	if s.oldID != "" {
		_ = m.store.Destroy(ctx, s.oldID)
	}
	save := s.modified || (!s.isNew && m.opt.Rolling)
	if s.isNew && !s.modified {
		save = false // nothing to remember: no row, no cookie
	}
	if !save {
		return
	}
	expires := time.Now().Add(m.opt.MaxAge)
	payload := map[string]any{}
	for k, v := range s.data {
		payload[k] = v
	}
	payload["cookie"] = map[string]any{
		"originalMaxAge": m.opt.MaxAge.Milliseconds(),
		"expires":        expires.UTC().Format("2006-01-02T15:04:05.000Z"),
		"secure":         m.opt.Secure,
		"httpOnly":       true,
		"path":           m.opt.Path,
		"sameSite":       "lax",
	}
	if err := m.store.Set(ctx, s.id, payload, m.opt.MaxAge); err != nil {
		return
	}
	if m.opt.Secure && !m.isSecureRequest(c.r) {
		return // a Secure cookie would never come back over plain http
	}
	http.SetCookie(c.ResponseWriter, &http.Cookie{
		Name:     m.opt.Name,
		Value:    EncodeCookieValue(s.id, m.opt.Secrets[0]),
		Path:     m.opt.Path,
		Expires:  expires,
		HttpOnly: true,
		Secure:   m.opt.Secure,
		SameSite: m.opt.SameSite,
	})
}

func (c *commitWriter) WriteHeader(code int) {
	c.commit()
	c.ResponseWriter.WriteHeader(code)
}

func (c *commitWriter) Write(b []byte) (int, error) {
	c.commit()
	return c.ResponseWriter.Write(b)
}

func (c *commitWriter) Flush() {
	c.commit()
	if f, ok := c.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (c *commitWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	c.commit()
	h, ok := c.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("sessionx: underlying writer does not support hijacking")
	}
	return h.Hijack()
}

// Unwrap lets http.ResponseController reach the real writer.
func (c *commitWriter) Unwrap() http.ResponseWriter { return c.ResponseWriter }
