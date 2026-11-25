package cron

import (
	"encoding/json"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	_ "time/tzdata"
)

type vectors struct {
	Validate []struct {
		Expr  string `json:"expr"`
		Valid bool   `json:"valid"`
	} `json:"validate"`
	Parse []struct {
		Expr  string  `json:"expr"`
		Error *string `json:"error"`
	} `json:"parse"`
	Next []struct {
		Expr string `json:"expr"`
		Zone string `json:"zone"`
		From string `json:"from"`
		Runs string `json:"runs"`
	} `json:"next"`
	Match []struct {
		Expr  string `json:"expr"`
		Zone  string `json:"zone"`
		At    string `json:"at"`
		Match bool   `json:"match"`
	} `json:"match"`
	Plan []struct {
		Expected string   `json:"expected"`
		Now      string   `json:"now"`
		Tol      int64    `json:"tol"`
		Missed   []string `json:"missed"`
		Run      *string  `json:"run"`
		Next     string   `json:"next"`
	} `json:"plan"`
}

func load(t *testing.T) vectors {
	t.Helper()
	raw, err := os.ReadFile("testdata/vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var v vectors
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	tm, err := time.Parse("2006-01-02T15:04:05.000Z", s)
	if err != nil {
		t.Fatal(err)
	}
	return tm
}

func iso(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

func TestValidateMatchesNodeCron(t *testing.T) {
	for _, c := range load(t).Validate {
		if got := Validate(c.Expr); got != c.Valid {
			t.Errorf("Validate(%q) = %v, node-cron says %v", c.Expr, got, c.Valid)
		}
	}
}

func TestParseErrorsMatchNodeCron(t *testing.T) {
	for _, c := range load(t).Parse {
		_, err := Parse(c.Expr)
		switch {
		case c.Error == nil && err != nil:
			t.Errorf("Parse(%q) failed: %v", c.Expr, err)
		case c.Error != nil && err == nil:
			t.Errorf("Parse(%q) succeeded, node-cron says %q", c.Expr, *c.Error)
		case c.Error != nil && err.Error() != *c.Error:
			t.Errorf("Parse(%q) error %q, node-cron says %q", c.Expr, err, *c.Error)
		}
	}
}

func TestNextMatchesNodeCron(t *testing.T) {
	for _, c := range load(t).Next {
		spec, err := Parse(c.Expr)
		if err != nil {
			t.Fatalf("%q: %v", c.Expr, err)
		}
		loc, err := time.LoadLocation(c.Zone)
		if err != nil {
			t.Fatal(err)
		}
		var runs []string
		from := mustTime(t, c.From)
		for range 3 {
			next, err := spec.Next(from, loc)
			if err != nil {
				runs = append(runs, "ERR:"+err.Error())
				break
			}
			runs = append(runs, iso(next))
			from = next
		}
		if got := strings.Join(runs, ","); got != c.Runs {
			t.Errorf("%q in %s from %s:\n got  %s\n want %s", c.Expr, c.Zone, c.From, got, c.Runs)
		}
	}
}

func TestMatchMatchesNodeCron(t *testing.T) {
	for _, c := range load(t).Match {
		spec, _ := Parse(c.Expr)
		loc, _ := time.LoadLocation(c.Zone)
		if got := spec.Match(mustTime(t, c.At), loc); got != c.Match {
			t.Errorf("%q in %s at %s: got %v want %v", c.Expr, c.Zone, c.At, got, c.Match)
		}
	}
}

func TestPlanBeatMatchesNodeCron(t *testing.T) {
	spec, _ := Parse("*/5 * * * *")
	nextMatch := func(d time.Time) (time.Time, error) { return spec.Next(d, time.UTC) }
	for _, c := range load(t).Plan {
		got, err := planBeat(mustTime(t, c.Expected), mustTime(t, c.Now), time.Duration(c.Tol)*time.Millisecond, nextMatch)
		if err != nil {
			t.Fatal(err)
		}
		var missed []string
		for _, m := range got.missed {
			missed = append(missed, iso(m))
		}
		if strings.Join(missed, ",") != strings.Join(c.Missed, ",") {
			t.Errorf("plan %+v: missed %v", c, missed)
		}
		switch {
		case got.run == nil && c.Run != nil, got.run != nil && (c.Run == nil || iso(*got.run) != *c.Run):
			t.Errorf("plan %+v: run %v", c, got.run)
		}
		if iso(got.next) != c.Next {
			t.Errorf("plan %+v: next %s", c, iso(got.next))
		}
	}
}

// ticker runs a job on a manual clock and records the slots it ran for.
type ticker struct {
	clock *ManualClock
	slots chan time.Time
}

func newTicker(start time.Time) *ticker {
	return &ticker{clock: NewManualClock(start), slots: make(chan time.Time, 64)}
}

func (k *ticker) fn(e Execution) { k.slots <- e.Slot }

func (k *ticker) expect(t *testing.T, want ...string) {
	t.Helper()
	// Runs are concurrent, so slots of one batch arrive in any order.
	pending := map[string]bool{}
	for _, w := range want {
		pending[w] = true
	}
	for range want {
		select {
		case got := <-k.slots:
			if !pending[iso(got)] {
				t.Fatalf("ran slot %s, want one of %v", iso(got), want)
			}
			delete(pending, iso(got))
		case <-time.After(2 * time.Second):
			t.Fatalf("slots %v never ran", pending)
		}
	}
	select {
	case got := <-k.slots:
		t.Fatalf("unexpected extra run for %s", iso(got))
	case <-time.After(30 * time.Millisecond):
	}
}

func TestJobRunsEverySlotOnTime(t *testing.T) {
	k := newTicker(time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC))
	job, err := Schedule("*/5 * * * *", k.fn, Options{Location: time.UTC, Clock: k.clock})
	if err != nil {
		t.Fatal(err)
	}
	defer job.Stop()
	if got := iso(job.Next()); got != "2026-06-01T12:05:00.000Z" {
		t.Fatalf("next %s", got)
	}
	k.clock.Advance(5 * time.Minute)
	k.expect(t, "2026-06-01T12:05:00.000Z")
	k.clock.Advance(10 * time.Minute)
	k.expect(t, "2026-06-01T12:10:00.000Z", "2026-06-01T12:15:00.000Z")
}

func TestJobStopEndsTheSchedule(t *testing.T) {
	k := newTicker(time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC))
	job, _ := Schedule("* * * * *", k.fn, Options{Location: time.UTC, Clock: k.clock})
	job.Stop()
	job.Stop()
	k.clock.Advance(time.Hour)
	k.expect(t)
	if job.Running() || !job.Next().IsZero() {
		t.Fatal("stopped job still reports a next run")
	}
}

func TestJobReportsAndSkipsMissedSlots(t *testing.T) {
	k := newTicker(time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC))
	var missed atomic.Int32
	job, _ := Schedule("* * * * *", k.fn, Options{
		Location: time.UTC, Clock: k.clock, MissedTolerance: 10 * time.Second,
		OnMissed: func(time.Time) { missed.Add(1) },
	})
	defer job.Stop()
	// The process stalls: time jumps a whole 3 minutes without the timers
	// firing in between, so only the newest slot is within tolerance.
	k.clock.now = k.clock.now.Add(3 * time.Minute)
	job.heartbeat()
	k.expect(t, "2026-06-01T12:03:00.000Z")
	deadline := time.Now().Add(2 * time.Second)
	for missed.Load() != 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if missed.Load() != 2 {
		t.Fatalf("missed slots reported: %d, want 2", missed.Load())
	}
}

func TestJobOverlapPolicy(t *testing.T) {
	for _, noOverlap := range []bool{false, true} {
		k := newTicker(time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC))
		release := make(chan struct{})
		var started, overlaps atomic.Int32
		job, _ := Schedule("* * * * *", func(e Execution) {
			started.Add(1)
			<-release
		}, Options{
			Location: time.UTC, Clock: k.clock, NoOverlap: noOverlap,
			OnOverlap: func(time.Time) { overlaps.Add(1) },
		})
		k.clock.Advance(time.Minute)
		waitFor(t, func() bool { return started.Load() == 1 })
		k.clock.Advance(time.Minute)
		waitFor(t, func() bool { return overlaps.Load() == 1 })
		want := int32(2)
		if noOverlap {
			want = 1
		}
		time.Sleep(30 * time.Millisecond)
		if started.Load() != want {
			t.Errorf("noOverlap=%v: %d runs started, want %d", noOverlap, started.Load(), want)
		}
		close(release)
		job.Stop()
	}
}

func TestJobStopsAfterMaxExecutions(t *testing.T) {
	k := newTicker(time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC))
	job, _ := Schedule("* * * * *", k.fn, Options{Location: time.UTC, Clock: k.clock, MaxExecutions: 2})
	k.clock.Advance(time.Minute)
	k.expect(t, "2026-06-01T12:01:00.000Z")
	k.clock.Advance(time.Minute)
	k.expect(t, "2026-06-01T12:02:00.000Z")
	waitFor(t, func() bool { return !job.Running() })
	k.clock.Advance(time.Hour)
	k.expect(t)
}

func TestStopDropsRunsWaitingOutRandomDelay(t *testing.T) {
	k := newTicker(time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC))
	job, _ := Schedule("* * * * *", k.fn, Options{Location: time.UTC, Clock: k.clock, MaxRandomDelay: time.Hour})
	k.clock.Advance(time.Minute)
	time.Sleep(30 * time.Millisecond)
	job.Stop()
	k.clock.Advance(2 * time.Hour)
	k.expect(t)
}

func TestScheduleRejectsInvalidExpression(t *testing.T) {
	if _, err := Schedule("61 * * * *", func(Execution) {}, Options{}); err == nil {
		t.Fatal("invalid expression accepted")
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
