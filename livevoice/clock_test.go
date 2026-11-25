package livevoice

import (
	"sort"
	"sync"
	"time"
)

// fakeClock runs timers in virtual time. Advance fires every timer that falls
// due, in order, including ones the callbacks arm.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
	seq    int
}

type fakeTimer struct {
	clock   *fakeClock
	at      time.Time
	seq     int
	f       func()
	stopped bool
}

func newFakeClock(start time.Time) *fakeClock { return &fakeClock{now: start} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) AfterFunc(d time.Duration, f func()) Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seq++
	t := &fakeTimer{clock: c, at: c.now.Add(d), seq: c.seq, f: f}
	c.timers = append(c.timers, t)
	return t
}

func (t *fakeTimer) Stop() bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	was := !t.stopped
	t.stopped = true
	return was
}

// Advance moves time forward by d.
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	target := c.now.Add(d)
	c.mu.Unlock()
	for {
		c.mu.Lock()
		var due []*fakeTimer
		for _, t := range c.timers {
			if !t.stopped && !t.at.After(target) {
				due = append(due, t)
			}
		}
		if len(due) == 0 {
			c.now = target
			c.mu.Unlock()
			return
		}
		sort.Slice(due, func(i, j int) bool {
			if !due[i].at.Equal(due[j].at) {
				return due[i].at.Before(due[j].at)
			}
			return due[i].seq < due[j].seq
		})
		next := due[0]
		next.stopped = true
		c.now = next.at
		c.mu.Unlock()
		next.f()
	}
}
