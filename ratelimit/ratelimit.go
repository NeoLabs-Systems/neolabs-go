// Package ratelimit is a fixed-window, per-client request limiter that answers
// exactly like express-rate-limit with `standardHeaders: true` (draft-6):
// RateLimit-Policy / RateLimit-Limit / RateLimit-Remaining / RateLimit-Reset,
// and on rejection a 429 JSON body plus Retry-After.
package ratelimit

import (
	"encoding/json"
	"math"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// Config describes one limiter.
type Config struct {
	Window  time.Duration
	Max     int
	Message any                        // JSON body of the 429 response
	Key     func(*http.Request) string // default: client IP
	Now     func() time.Time           // for tests
}

type entry struct {
	hits  int
	reset time.Time
}

// Limiter holds the counters. Create one per route group.
type Limiter struct {
	cfg  Config
	mu   sync.Mutex
	seen map[string]*entry
	last time.Time
}

// New returns a limiter. Window and Max must be positive.
func New(cfg Config) *Limiter {
	if cfg.Window <= 0 || cfg.Max <= 0 {
		panic("ratelimit: Window and Max must be positive")
	}
	if cfg.Key == nil {
		cfg.Key = ClientIP
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Message == nil {
		cfg.Message = map[string]string{"error": "Too many requests, please try again later."}
	}
	return &Limiter{cfg: cfg, seen: map[string]*entry{}}
}

// ClientIP is the remote address without the port. Behind a trusted proxy,
// wrap the handler with a middleware that rewrites RemoteAddr first.
func ClientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (l *Limiter) hit(key string) (hits int, reset time.Time) {
	now := l.cfg.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.Sub(l.last) > l.cfg.Window { // bound memory: drop expired entries
		for k, e := range l.seen {
			if !now.Before(e.reset) {
				delete(l.seen, k)
			}
		}
		l.last = now
	}
	e := l.seen[key]
	if e == nil || !now.Before(e.reset) {
		e = &entry{reset: now.Add(l.cfg.Window)}
		l.seen[key] = e
	}
	e.hits++
	return e.hits, e.reset
}

// Middleware enforces the limit.
func (l *Limiter) Middleware(next http.Handler) http.Handler {
	windowSecs := int(l.cfg.Window / time.Second)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits, reset := l.hit(l.cfg.Key(r))
		remaining := l.cfg.Max - hits
		if remaining < 0 {
			remaining = 0
		}
		resetSecs := int(math.Ceil(reset.Sub(l.cfg.Now()).Seconds()))
		if resetSecs < 0 {
			resetSecs = 0
		}
		h := w.Header()
		h.Set("RateLimit-Policy", strconv.Itoa(l.cfg.Max)+";w="+strconv.Itoa(windowSecs))
		h.Set("RateLimit-Limit", strconv.Itoa(l.cfg.Max))
		h.Set("RateLimit-Remaining", strconv.Itoa(remaining))
		h.Set("RateLimit-Reset", strconv.Itoa(resetSecs))
		if hits > l.cfg.Max {
			h.Set("Retry-After", strconv.Itoa(resetSecs))
			h.Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusTooManyRequests)
			_ = json.NewEncoder(w).Encode(l.cfg.Message)
			return
		}
		next.ServeHTTP(w, r)
	})
}
