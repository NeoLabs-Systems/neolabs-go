package cron

import (
	"sort"
	"sync"
	"time"
)

// Timer is a pending call that can be cancelled.
type Timer interface{ Stop() bool }

// Clock is the time source of a scheduler. The real clock is the default;
// ManualClock drives schedulers in tests.
type Clock interface {
	Now() time.Time
	AfterFunc(d time.Duration, f func()) Timer
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

func (realClock) AfterFunc(d time.Duration, f func()) Timer { return time.AfterFunc(d, f) }

// ManualClock is a clock that only moves when Advance is called, firing the
// timers that fall due on the way, in order.
type ManualClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*manualTimer
}

type manualTimer struct {
	clock *ManualClock
	at    time.Time
	f     func()
	done  bool
}

// NewManualClock starts a manual clock at start.
func NewManualClock(start time.Time) *ManualClock { return &ManualClock{now: start} }

// Now is the clock's current time.
func (c *ManualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// AfterFunc schedules f to run when the clock reaches now+d.
func (c *ManualClock) AfterFunc(d time.Duration, f func()) Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &manualTimer{clock: c, at: c.now.Add(d), f: f}
	c.timers = append(c.timers, t)
	return t
}

func (t *manualTimer) Stop() bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	was := !t.done
	t.done = true
	return was
}

// Advance moves the clock forward by d. Each timer due on the way runs with the
// clock set to its own time, and timers it schedules inside the span run too.
func (c *ManualClock) Advance(d time.Duration) {
	c.mu.Lock()
	target := c.now.Add(d)
	c.mu.Unlock()
	for {
		c.mu.Lock()
		sort.SliceStable(c.timers, func(i, j int) bool { return c.timers[i].at.Before(c.timers[j].at) })
		var next *manualTimer
		for _, t := range c.timers {
			if !t.done && !t.at.After(target) {
				next = t
				break
			}
		}
		if next == nil {
			c.now = target
			c.timers = c.pending()
			c.mu.Unlock()
			return
		}
		next.done = true
		if next.at.After(c.now) {
			c.now = next.at
		}
		c.mu.Unlock()
		next.f()
	}
}

func (c *ManualClock) pending() []*manualTimer {
	var out []*manualTimer
	for _, t := range c.timers {
		if !t.done {
			out = append(out, t)
		}
	}
	return out
}
