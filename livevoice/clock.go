package livevoice

import "time"

// Timer is a pending callback that can be cancelled.
type Timer interface{ Stop() bool }

// Clock is the time source of the call logic, so tests can run the long
// waits of a call (a six second quiet line, a three minute idle hang-up) in
// virtual time.
type Clock interface {
	Now() time.Time
	AfterFunc(d time.Duration, f func()) Timer
}

// RealClock is the wall clock.
type RealClock struct{}

// Now is time.Now.
func (RealClock) Now() time.Time { return time.Now() }

// AfterFunc is time.AfterFunc.
func (RealClock) AfterFunc(d time.Duration, f func()) Timer { return time.AfterFunc(d, f) }

func stop(t Timer) {
	if t != nil {
		t.Stop()
	}
}
