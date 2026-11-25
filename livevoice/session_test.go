package livevoice

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

type fakeAdapter struct {
	mu       sync.Mutex
	handlers Handlers
	audio    [][]byte
	said     []string
	noted    []string
	closed   bool
	failWith error
	turnEnds bool
}

func (a *fakeAdapter) Connect(context.Context, ConnectParams) error { return a.failWith }
func (a *fakeAdapter) AppendAudio(pcm []byte) {
	a.mu.Lock()
	a.audio = append(a.audio, pcm)
	a.mu.Unlock()
}
func (a *fakeAdapter) StartInput()        {}
func (a *fakeAdapter) EndInput()          {}
func (a *fakeAdapter) Say(text, _ string) { a.mu.Lock(); a.said = append(a.said, text); a.mu.Unlock() }
func (a *fakeAdapter) Note(text, _ string) {
	a.mu.Lock()
	a.noted = append(a.noted, text)
	a.mu.Unlock()
}
func (a *fakeAdapter) Close()                { a.mu.Lock(); a.closed = true; a.mu.Unlock() }
func (a *fakeAdapter) ReportsTurnEnds() bool { return a.turnEnds }
func (a *fakeAdapter) Said() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.said...)
}

type fakeTasks struct {
	mu        sync.Mutex
	started   []string
	hasRuns   bool
	report    *StatusReport
	onStart   func(runID string)
	startedAt []string
}

func (f *fakeTasks) Delegate(handle, request string) (Delegation, bool) {
	if request == "" {
		return Delegation{}, false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	id := fmt.Sprint(len(f.started) + 1)
	f.started = append(f.started, request)
	f.hasRuns = true
	return Delegation{TaskID: id, RunID: "run-" + id, Start: func() {
		if f.onStart != nil {
			f.onStart("run-" + id)
		}
	}}, true
}
func (f *fakeTasks) RunTool(name string, args map[string]any) map[string]any {
	return map[string]any{"tool": name, "args": args}
}
func (f *fakeTasks) Running() []RunningTask      { return nil }
func (f *fakeTasks) ActiveRunID() string         { return "" }
func (f *fakeTasks) HasRunningWork() bool        { f.mu.Lock(); defer f.mu.Unlock(); return f.hasRuns }
func (f *fakeTasks) HasRuns() bool               { f.mu.Lock(); defer f.mu.Unlock(); return f.hasRuns }
func (f *fakeTasks) StatusReport() *StatusReport { f.mu.Lock(); defer f.mu.Unlock(); return f.report }

type fakeStore struct {
	mu      sync.Mutex
	turns   []string
	replies []string
}

func (s *fakeStore) RecordTurn(role, content string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.turns = append(s.turns, role+": "+content)
	return nil
}
func (s *fakeStore) RecordTaskReply(runID, content string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.replies = append(s.replies, runID+": "+content)
	return nil
}

type sentEvent struct {
	kind    string
	payload map[string]any
}

type fakeSink struct {
	mu     sync.Mutex
	events []sentEvent
}

func (s *fakeSink) Send(kind string, payload map[string]any) {
	s.mu.Lock()
	s.events = append(s.events, sentEvent{kind, payload})
	s.mu.Unlock()
}

func (s *fakeSink) kinds() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.events))
	for i, e := range s.events {
		out[i] = e.kind
		if e.kind == "state" {
			out[i] += ":" + fmt.Sprint(e.payload["state"])
		}
	}
	return out
}

func (s *fakeSink) count(kind string) int {
	n := 0
	for _, k := range s.kinds() {
		if k == kind {
			n++
		}
	}
	return n
}

type rig struct {
	mu       sync.Mutex
	session  *Session
	clock    *fakeClock
	sink     *fakeSink
	tasks    *fakeTasks
	store    *fakeStore
	adapters []*fakeAdapter
	failNext []error
	idle     chan string
	hangUp   chan struct{}
}

func newRig(t *testing.T) *rig {
	t.Helper()
	r := &rig{clock: newFakeClock(time.UnixMilli(1_000_000)), sink: &fakeSink{}, tasks: &fakeTasks{}, store: &fakeStore{},
		idle: make(chan string, 4), hangUp: make(chan struct{}, 4)}
	r.session = NewSession(Config{
		ID: "s1", Provider: Provider{ID: "google", InputSampleRate: 16000, OutputSampleRate: 24000},
		Settings: Settings{Model: "m", Voice: "v", InputMode: "hands_free"},
		Sink:     r.sink, Tasks: r.tasks, Store: r.store, Clock: r.clock, DelegateTool: "run_task",
		NewAdapter: func(h Handlers) Adapter {
			a := &fakeAdapter{handlers: h}
			r.mu.Lock()
			defer r.mu.Unlock()
			if len(r.failNext) > 0 {
				a.failWith, r.failNext = r.failNext[0], r.failNext[1:]
			}
			r.adapters = append(r.adapters, a)
			return a
		},
		BuildPrompt: func(context.Context) (Prompt, error) { return Prompt{Instructions: "be brief"}, nil },
		Texts: Texts{
			TaskStarted: "started", NoAnswer: "no answer", EmptyRequest: "empty", HandoffWithoutRequest: "no request",
			Unanswered:   "say something",
			StatusUpdate: func(seconds int, facts string) string { return fmt.Sprintf("status after %d: %s", seconds, facts) },
			LineDropped:  func(last string) string { return "dropped: " + last },
		},
		ErrorFor: func(err error) string { return "E:" + err.Error() },
		OnIdle:   func(reason string) { r.idle <- reason },
		OnHangUp: func() { r.hangUp <- struct{}{} },
	})
	return r
}

func (r *rig) current() *fakeAdapter {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.adapters[len(r.adapters)-1]
}

func (r *rig) adapterCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.adapters)
}

func (r *rig) failConnects(errs ...error) {
	r.mu.Lock()
	r.failNext = errs
	r.mu.Unlock()
}

func TestConnectFlushesEarlyAudioAndAnnouncesReady(t *testing.T) {
	r := newRig(t)
	r.session.AppendAudio([]byte{1, 2})
	if err := r.session.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := r.current().audio; len(got) != 1 || got[0][0] != 1 {
		t.Fatalf("early audio = %v", got)
	}
	kinds := r.sink.kinds()
	want := []string{"session_ready", "state:listening"} // the call starts out "connecting"
	for i, k := range want {
		if i >= len(kinds) || kinds[i] != k {
			t.Fatalf("events = %v, want prefix %v", kinds, want)
		}
	}
	ready := r.sink.events[0].payload
	if ready["sessionId"] != "s1" || ready["outputSampleRate"] != 24000 || ready["activeRunId"] != nil || ready["reconnected"] != false {
		t.Fatalf("session_ready = %v", ready)
	}
}

func TestSpeechTranscriptsAndStoredTurns(t *testing.T) {
	r := newRig(t)
	_ = r.session.Connect(context.Background())
	h := r.current().handlers
	h.OnInputTranscript("Hello")
	h.OnInputTranscript("there.")
	h.OnOutputTranscript("Hi,")
	h.OnAudio(make([]byte, 4800))
	if got := r.sink.count("state:speaking"); got != 1 {
		t.Fatalf("speaking states = %d", got)
	}
	h.OnOutputTranscript("how are you?")
	h.OnTurnComplete(true)
	r.clock.Advance(speakingIdle)

	r.store.mu.Lock()
	defer r.store.mu.Unlock()
	if len(r.store.turns) != 2 || r.store.turns[0] != "user: Hello there." || r.store.turns[1] != "assistant: Hi, how are you?" {
		t.Fatalf("stored turns = %q", r.store.turns)
	}
	if got := r.sink.kinds(); got[len(got)-1] != "state:listening" {
		t.Fatalf("last event = %v", got)
	}
}

func TestToolCallStartsTaskAfterAnnouncingIt(t *testing.T) {
	r := newRig(t)
	_ = r.session.Connect(context.Background())
	var order []string
	r.tasks.onStart = func(runID string) {
		// The client already knows the task when its run starts.
		r.sink.mu.Lock()
		last := r.sink.events[len(r.sink.events)-1]
		r.sink.mu.Unlock()
		order = append(order, last.kind+":"+fmt.Sprint(last.payload["status"]))
	}
	h := r.current().handlers
	resp, ok := h.OnToolCall(ToolCall{ID: "c1", Name: "run_task", Args: map[string]any{"request": "  check mail "}})
	if !ok || resp.(map[string]any)["task_id"] != "1" || resp.(map[string]any)["result"] != "started" {
		t.Fatalf("response = %v", resp)
	}
	if len(order) != 1 || order[0] != "task:running" || r.tasks.started[0] != "check mail" {
		t.Fatalf("order = %v started = %q", order, r.tasks.started)
	}
	if resp, _ := h.OnToolCall(ToolCall{ID: "c2", Name: "run_task", Args: map[string]any{"request": " "}}); resp.(map[string]any)["error"] != "empty" {
		t.Fatalf("empty request answered %v", resp)
	}
	if resp, _ := h.OnToolCall(ToolCall{ID: "c3", Name: "check_tasks", Args: map[string]any{}}); resp.(map[string]any)["tool"] != "check_tasks" {
		t.Fatalf("task tool answered %v", resp)
	}
}

func TestTellWaitsForAFreeLine(t *testing.T) {
	r := newRig(t)
	_ = r.session.Connect(context.Background())
	a, h := r.current(), r.current().handlers
	h.OnAudio(make([]byte, 4800))
	r.session.Say("news")
	if len(a.Said()) != 0 {
		t.Fatal("told while the model was speaking")
	}
	h.OnTurnComplete(true)
	r.clock.Advance(speakingIdle)
	if got := a.Said(); len(got) != 1 || got[0] != "news" {
		t.Fatalf("said = %v", got)
	}
}

func TestSilentModelIsAskedAgainThenReconnected(t *testing.T) {
	r := newRig(t)
	_ = r.session.Connect(context.Background())
	r.session.Say("deliver this")
	if len(r.current().Said()) != 1 {
		t.Fatal("not told on a free line")
	}
	r.clock.Advance(ReplyTimeout) // heard nothing: ask again once
	if got := r.current().Said(); len(got) != 2 || got[1] != "deliver this" {
		t.Fatalf("said = %v", got)
	}
	r.clock.Advance(ReplyTimeout) // silent again on an open line: a fresh connection gets it
	waitFor(t, func() bool { return r.adapterCount() == 2 })
	waitFor(t, func() bool { return r.sink.count("session_ready") == 2 })
	waitFor(t, func() bool { return len(r.current().Said()) == 1 })
	if got := r.current().Said()[0]; got != "deliver this" {
		t.Fatalf("new connection told %q", got)
	}
}

func TestReconnectGivesUpAfterThreeFailures(t *testing.T) {
	r := newRig(t)
	_ = r.session.Connect(context.Background())
	r.failConnects(errors.New("down"), errors.New("down"), errors.New("down"), errors.New("down"))
	r.current().handlers.OnClosed("connection_lost", "")
	waitFor(t, func() bool { return r.sink.count("error") == 1 })
	var last map[string]any
	r.sink.mu.Lock()
	for _, e := range r.sink.events {
		if e.kind == "error" {
			last = e.payload
		}
	}
	r.sink.mu.Unlock()
	if last["recoverable"] != false || last["error"] != "E:The live voice connection ended (down)." {
		t.Fatalf("error = %v", last)
	}
	if r.session.Attached() {
		t.Fatal("still attached after giving up")
	}
}

func TestHangUpDropsTheLineAfterTheGoodbye(t *testing.T) {
	r := newRig(t)
	_ = r.session.Connect(context.Background())
	h := r.current().handlers
	h.OnAudio(make([]byte, 48000)) // one second of goodbye
	h.OnHangUp()
	r.clock.Advance(500 * time.Millisecond)
	select {
	case <-r.hangUp:
		t.Fatal("hung up before the goodbye played out")
	default:
	}
	r.clock.Advance(1200 * time.Millisecond)
	select {
	case <-r.hangUp:
	case <-time.After(time.Second):
		t.Fatal("never hung up")
	}
	if r.sink.count("state:closed") != 0 && r.session.Attached() {
		t.Fatal("sink still attached")
	}
}

func TestDetachKeepsUnheardOutcomesInTheChat(t *testing.T) {
	r := newRig(t)
	_ = r.session.Connect(context.Background())
	r.current().handlers.OnAudio(make([]byte, 480))
	r.session.TellOutcome("", "The task is finished", "run-7", "all done")
	r.session.Detach()
	r.store.mu.Lock()
	defer r.store.mu.Unlock()
	if len(r.store.replies) != 1 || r.store.replies[0] != "run-7: all done" {
		t.Fatalf("replies = %v", r.store.replies)
	}
	if !r.current().closed {
		t.Fatal("provider connection left open")
	}
}

func TestIdleCallHangsUp(t *testing.T) {
	r := newRig(t)
	_ = r.session.Connect(context.Background())
	r.clock.Advance(idleHangup + time.Second)
	select {
	case reason := <-r.idle:
		if reason != "idle_timeout" {
			t.Fatalf("reason = %q", reason)
		}
	case <-time.After(time.Second):
		t.Fatal("idle call never released")
	}
}

func TestStatusUpdatesGrowSparser(t *testing.T) {
	r := newRig(t)
	_ = r.session.Connect(context.Background())
	r.tasks.report = &StatusReport{Facts: "- working", LatestStep: "step 1"}
	r.tasks.hasRuns = true
	r.session.PublishTask("run-1", "running", "do it")
	a := r.current()
	r.clock.Advance(9 * time.Second)
	if len(a.Said()) != 0 {
		t.Fatal("status before the first gap")
	}
	r.clock.Advance(2 * time.Second)
	if got := a.Said(); len(got) != 1 || got[0] != "status after 10: - working" {
		t.Fatalf("said = %v", got)
	}
	// The model answers; the next status waits for a longer silence.
	a.handlers.OnAudio(make([]byte, 480))
	a.handlers.OnTurnComplete(true)
	r.clock.Advance(speakingIdle)
	r.tasks.report = &StatusReport{Facts: "- still working", LatestStep: "step 2"}
	r.clock.Advance(15 * time.Second)
	if len(a.Said()) != 1 {
		t.Fatalf("second status came after 15s: %v", a.Said())
	}
	r.clock.Advance(10 * time.Second)
	if len(a.Said()) != 2 {
		t.Fatalf("no second status after 25s: %v", a.Said())
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition never became true")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
