package cron

import (
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultMissedTolerance is how late a tick may start and still run.
const DefaultMissedTolerance = time.Second

// maxDelay caps one sleep, so a far-off next run is re-planned daily.
const maxDelay = 24 * time.Hour

// Execution is what a job function is told about its run.
type Execution struct {
	// Slot is the scheduled second this run is for (not the moment it started).
	Slot time.Time
}

// Options tune a job. The zero value is node-cron's default: local zone, one
// second of tolerance, overlapping runs allowed, no random delay, no limit.
type Options struct {
	// Location is the zone the expression is read in; nil is the local zone.
	Location *time.Location
	// MissedTolerance is how late after its slot a tick may still run. A tick
	// later than this is reported to OnMissed and skipped. Zero means
	// DefaultMissedTolerance.
	MissedTolerance time.Duration
	// NoOverlap drops a tick while the previous run is still going.
	NoOverlap bool
	// MaxRandomDelay delays each run by a random time below it.
	MaxRandomDelay time.Duration
	// MaxExecutions stops the job after that many completed runs; 0 is no limit.
	MaxExecutions int
	// OnMissed is called for a slot that was skipped because it came too late.
	OnMissed func(slot time.Time)
	// OnOverlap is called for a tick that arrives while the previous run is
	// still going (whether or not NoOverlap then drops it).
	OnOverlap func(slot time.Time)
	// Logger receives job panics and the overlap warning; nil logs nothing.
	Logger *slog.Logger
	// Clock is the time source; nil is the real clock.
	Clock Clock
}

// Job is a running schedule.
type Job struct {
	schedule *Spec
	fn       func(Execution)
	opts     Options
	clock    Clock

	mu       sync.Mutex
	running  bool
	expected time.Time
	timer    Timer
	stopCh   chan struct{}
	last     *run
	count    int
}

type run struct{ done atomic.Bool }

// Schedule parses expression and starts calling fn on every slot, like
// node-cron's schedule(). Stop it with Job.Stop.
func Schedule(expression string, fn func(Execution), opts Options) (*Job, error) {
	schedule, err := Parse(expression)
	if err != nil {
		return nil, err
	}
	if opts.MissedTolerance == 0 {
		opts.MissedTolerance = DefaultMissedTolerance
	}
	if opts.Clock == nil {
		opts.Clock = realClock{}
	}
	j := &Job{schedule: schedule, fn: fn, opts: opts, clock: opts.Clock, stopCh: make(chan struct{})}
	if err := j.start(); err != nil {
		return nil, err
	}
	return j, nil
}

func (j *Job) nowWithoutMs() time.Time { return j.clock.Now().Truncate(time.Second) }

func (j *Job) start() error {
	next, err := j.schedule.Next(j.nowWithoutMs(), j.opts.Location)
	if err != nil {
		return err
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	j.running = true
	j.expected = next
	j.arm()
	return nil
}

// arm schedules the next heartbeat. The caller holds j.mu.
func (j *Job) arm() {
	if !j.running {
		return
	}
	if j.timer != nil {
		j.timer.Stop()
	}
	delay := min(max(j.expected.Sub(j.clock.Now()), 0), maxDelay)
	j.timer = j.clock.AfterFunc(delay, j.heartbeat)
}

// Beat is what one heartbeat decided: slots to report as missed, the slot to
// run (nil when none is due) and the slot to wait for next.
type beat struct {
	missed []time.Time
	run    *time.Time
	next   time.Time
}

// planBeat decides what a heartbeat at now does, given the slot it expected.
// A slot within the tolerance (and before the one after it) runs; slots that
// are later are missed.
func planBeat(expected, now time.Time, tolerance time.Duration, nextMatch func(time.Time) (time.Time, error)) (beat, error) {
	var b beat
	slot := expected
	for {
		if now.Before(slot) {
			b.next = slot
			return b, nil
		}
		next, err := nextMatch(slot)
		if err != nil {
			return b, err
		}
		if !next.After(slot) {
			b.next, err = nextMatch(now)
			return b, err
		}
		gap := next.Sub(slot)
		lateBy := now.Sub(slot)
		if lateBy <= tolerance && lateBy < gap {
			s := slot
			b.run = &s
			b.next = next
			return b, nil
		}
		b.missed = append(b.missed, slot)
		slot = next
	}
}

func (j *Job) nextMatch(after time.Time) (time.Time, error) {
	return j.schedule.Next(after, j.opts.Location)
}

func (j *Job) heartbeat() {
	j.mu.Lock()
	if !j.running {
		j.mu.Unlock()
		return
	}
	plan, err := planBeat(j.expected, j.nowWithoutMs(), j.opts.MissedTolerance, j.nextMatch)
	if err != nil {
		// No later slot exists: the job has nothing left to wait for.
		j.halt()
		j.mu.Unlock()
		j.logError("cron job stopped: no next run", err)
		return
	}
	j.expected = plan.next
	for _, slot := range plan.missed {
		if j.opts.OnMissed != nil {
			go j.opts.OnMissed(slot)
		}
	}
	if plan.run != nil {
		slot := *plan.run
		if j.last != nil && !j.last.done.Load() {
			if j.opts.OnOverlap != nil {
				go j.opts.OnOverlap(slot)
			}
			if j.opts.NoOverlap {
				j.logWarn("task still running, new execution blocked by overlap prevention")
				j.arm()
				j.mu.Unlock()
				return
			}
		}
		r := &run{}
		j.last = r
		go j.execute(slot, r)
	}
	j.arm()
	j.mu.Unlock()
}

func (j *Job) execute(slot time.Time, r *run) {
	defer r.done.Store(true)
	if j.opts.MaxRandomDelay > 0 {
		if delay := time.Duration(rand.Int64N(int64(j.opts.MaxRandomDelay))); delay > 0 && !j.wait(delay) {
			return
		}
	}
	j.call(slot)
}

// wait sleeps for d and reports whether the job is still running afterwards;
// Stop cancels a pending delay and the run with it.
func (j *Job) wait(d time.Duration) bool {
	fired := make(chan struct{})
	t := j.clock.AfterFunc(d, func() { close(fired) })
	select {
	case <-fired:
		return true
	case <-j.stopCh:
		t.Stop()
		return false
	}
}

func (j *Job) call(slot time.Time) {
	defer func() {
		if p := recover(); p != nil {
			j.logError("cron task failed", fmt.Errorf("panic: %v", p))
		}
	}()
	j.fn(Execution{Slot: slot})
	j.mu.Lock()
	j.count++
	reached := j.opts.MaxExecutions > 0 && j.count >= j.opts.MaxExecutions
	j.mu.Unlock()
	if reached {
		j.Stop()
	}
}

func (j *Job) logError(msg string, err error) {
	if j.opts.Logger != nil {
		j.opts.Logger.Error(msg, "expression", j.schedule.expression, "error", err)
	}
}

func (j *Job) logWarn(msg string) {
	if j.opts.Logger != nil {
		j.opts.Logger.Warn(msg, "expression", j.schedule.expression)
	}
}

// Stop ends the schedule: no further slot runs, and runs still waiting out
// their random delay are dropped. A run already executing is left to finish.
func (j *Job) Stop() {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.halt()
}

// halt stops the job; the caller holds j.mu.
func (j *Job) halt() {
	if j.timer != nil {
		j.timer.Stop()
		j.timer = nil
	}
	if j.running {
		j.running = false
		close(j.stopCh)
	}
}

// Next is the slot the job waits for next, or the zero time once stopped.
func (j *Job) Next() time.Time {
	j.mu.Lock()
	defer j.mu.Unlock()
	if !j.running {
		return time.Time{}
	}
	return j.expected
}

// Running reports whether the job still schedules runs.
func (j *Job) Running() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.running
}
