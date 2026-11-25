package cli

import (
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"time"
)

var spinner = []string{"◐", "◓", "◑", "◒"}

// FormatDuration renders a short human duration such as "4s" or "1m 5s"
// (at least 1s).
func FormatDuration(d time.Duration) string {
	s := max(1, int((d+500*time.Millisecond)/time.Second))
	if s < 60 {
		return fmt.Sprintf("%ds", s)
	}
	return fmt.Sprintf("%dm %ds", s/60, s%60)
}

// Label is a step's text while it runs and once it is done.
type Label struct{ Doing, Done string }

type step struct {
	Label
	state, detail string
	started       time.Time
}

// StepList is a checklist of what a command is doing, drawn in place while it
// runs:
//
//	━━━━━━━━━━━━━━──────────  2 of 5
//	✓ Saved a backup                      1s
//	◐ Installing…  getting everything ready
//	○ Restart NeoAgent
//
// When every step succeeds the list folds away and the command prints its own
// one-line result; a failure leaves it up so the failed step stays visible.
// Without a terminal to redraw (logs, pipes) each step prints once it ends.
type StepList struct {
	u     *UI
	steps []*step
	bar   bool
	live  bool
	drawn []int
	frame int

	mu     sync.Mutex
	ticker *time.Ticker
	stop   chan struct{}
	resize chan os.Signal
}

// Steps shows a step list; bar adds the progress bar above it. Close it when
// the command is done.
func (u *UI) Steps(labels []Label, bar bool) *StepList {
	l := &StepList{u: u, bar: bar, live: u.CanRedraw()}
	for _, label := range labels {
		l.steps = append(l.steps, &step{Label: label, state: "pending"})
	}
	if l.live {
		u.live, u.steps = l, l
		l.resize = make(chan os.Signal, 1)
		notifyResize(l.resize)
		go func() {
			for range l.resize {
				l.mu.Lock()
				l.redraw()
				l.mu.Unlock()
			}
		}()
		l.draw()
	} else if !u.Silent {
		u.steps = l
	}
	return l
}

// Step is one step of RunSteps.
type Step struct {
	Label
	Run func() error
}

// RunSteps runs steps in a step list and closes it.
func (u *UI) RunSteps(bar bool, steps ...Step) error {
	labels := make([]Label, len(steps))
	for i, s := range steps {
		labels[i] = s.Label
	}
	l := u.Steps(labels, bar)
	defer l.Close()
	for i, s := range steps {
		if err := l.Run(i, s.Run); err != nil {
			return err
		}
	}
	return nil
}

// Step runs one step in its own step list.
func (u *UI) Step(doing, done string, fn func() error) error {
	return u.RunSteps(false, Step{Label{doing, done}, fn})
}

// Add appends a step and returns its index.
func (l *StepList) Add(label Label) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.steps = append(l.steps, &step{Label: label, state: "pending"})
	l.redraw()
	return len(l.steps) - 1
}

// Doing is the running label of step i.
func (l *StepList) Doing(i int) string { return l.steps[i].Doing }

// Note sets the running step's detail; it reports false when no step runs.
func (l *StepList) Note(text string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, s := range l.steps {
		if s.state == "running" {
			s.detail = strings.Join(strings.Fields(text), " ")
			l.redraw()
			return true
		}
	}
	return false
}

// Run runs step i.
func (l *StepList) Run(i int, fn func() error) error {
	l.start(i)
	if err := fn(); err != nil {
		l.finish(i, "error")
		return err
	}
	l.finish(i, "ok")
	return nil
}

func (l *StepList) start(i int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := l.steps[i]
	s.state, s.detail, s.started = "running", "", time.Now()
	if l.live && l.ticker == nil {
		l.ticker, l.stop = time.NewTicker(120*time.Millisecond), make(chan struct{})
		go func(t *time.Ticker, stop chan struct{}) {
			for {
				select {
				case <-t.C:
					l.mu.Lock()
					l.frame = (l.frame + 1) % len(spinner)
					l.redraw()
					l.mu.Unlock()
				case <-stop:
					return
				}
			}
		}(l.ticker, l.stop)
	}
	l.redraw()
}

func (l *StepList) finish(i int, state string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := l.steps[i]
	s.state, s.detail = state, ""
	if state == "ok" {
		s.detail = FormatDuration(time.Since(s.started))
	}
	running := false
	for _, other := range l.steps {
		running = running || other.state == "running"
	}
	if !running {
		l.stopTimer()
	}
	if l.live {
		l.redraw()
	} else if !l.u.Silent {
		fmt.Fprintln(l.u.Out, l.u.fit(l.lineFor(s)))
	}
}

// Close folds the list away when every step succeeded, and otherwise leaves
// it on screen.
func (l *StepList) Close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.stopTimer()
	if l.u.steps == l {
		l.u.steps = nil
	}
	if !l.live {
		return
	}
	allOK := true
	for _, s := range l.steps {
		allOK = allOK && s.state == "ok"
	}
	if allOK {
		l.clear()
	} else {
		l.redraw()
	}
	if l.u.live == l {
		l.u.live = nil
	}
	signal.Stop(l.resize)
	close(l.resize)
	l.live = false
}

func (l *StepList) stopTimer() {
	if l.ticker != nil {
		l.ticker.Stop()
		close(l.stop)
		l.ticker = nil
	}
}

func (l *StepList) lineFor(s *step) string {
	u := l.u
	switch s.state {
	case "running":
		detail := ""
		if s.detail != "" {
			detail = "  " + u.Muted(s.detail)
		}
		return "  " + u.Accent(spinner[l.frame]) + " " + u.Bold(s.Doing+"…") + detail
	case "ok":
		// Durations line up on screen; in a log they follow the label directly.
		width := 0
		if l.live {
			width = min(40, u.Width()-14)
		}
		return u.ItemLine("ok", s.Done, s.detail, width)
	case "error":
		return u.ItemLine("error", s.Doing, "", 0)
	}
	return "  " + u.Faint("○ "+s.Doing)
}

func (l *StepList) barLine() string {
	u := l.u
	done := 0
	for _, s := range l.steps {
		if s.state == "ok" {
			done++
		}
	}
	size := max(10, min(32, u.Width()-16))
	filled := int(float64(done)/float64(len(l.steps))*float64(size) + 0.5)
	return "  " + u.Accent(strings.Repeat("━", filled)) + u.SGR(u.Foreground("unlit"), strings.Repeat("━", size-filled)) + "  " +
		u.Faint(fmt.Sprintf("%d of %d", done, len(l.steps)))
}

func (l *StepList) printAbove(write func()) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.clear()
	write()
	l.draw()
}

// draw, clear and redraw run with l.mu held.
func (l *StepList) draw() {
	lines := make([]string, 0, len(l.steps)+2)
	if l.bar {
		lines = append(lines, l.barLine(), "")
	}
	for _, s := range l.steps {
		lines = append(lines, l.lineFor(s))
	}
	l.drawn = l.u.drawBlock(lines)
}

func (l *StepList) clear() {
	l.u.clearBlock(l.drawn)
	l.drawn = nil
}

func (l *StepList) redraw() {
	if !l.live {
		return
	}
	l.clear()
	l.draw()
}
