package wsgate

import (
	"sync"
	"time"
)

// attemptLimiter counts upgrade attempts per client address in fixed windows,
// like createUpgradeLimiter of the Node gateways.
type attemptLimiter struct {
	window time.Duration
	max    int

	mu      sync.Mutex
	entries map[string]*attempts
	sweeps  int
}

type attempts struct {
	start time.Time
	count int
}

func newAttemptLimiter(window time.Duration, max int) *attemptLimiter {
	return &attemptLimiter{window: window, max: max, entries: map[string]*attempts{}}
}

func (l *attemptLimiter) allow(key string) bool {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.sweeps++; l.sweeps%256 == 0 {
		for k, e := range l.entries {
			if now.Sub(e.start) >= l.window {
				delete(l.entries, k)
			}
		}
	}
	e := l.entries[key]
	if e == nil || now.Sub(e.start) >= l.window {
		l.entries[key] = &attempts{start: now, count: 1}
		return true
	}
	if e.count >= l.max {
		return false
	}
	e.count++
	return true
}
