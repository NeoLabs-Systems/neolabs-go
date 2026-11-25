package livevoice

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/NeoLabs-Systems/neolabs-go/audiox"
)

// Timings of a call, as the Node server had them.
const (
	maxPendingAudioBytes = 5 * 24000 * 2
	speakingIdle         = 700 * time.Millisecond
	// GPT-Live announces a hand-off before the owner's transcript has fully
	// arrived; waiting briefly lets the request carry the whole utterance.
	delegationSettle     = 500 * time.Millisecond
	maxReconnectAttempts = 3
	// A call nobody has spoken on for this long, with no task running, was left
	// open by accident; hanging up ends the billed provider session.
	idleHangup = 3 * time.Minute
	// Margin past the end of the model's goodbye before the line drops,
	// covering the client's playback buffer.
	hangupGrace = 600 * time.Millisecond
	// Spoken progress stays occasional on a long task; the updates in between
	// still reach the model as context.
	spokenProgressGap = 60 * time.Second
	statusTick        = time.Second
)

// statusSilenceGaps: while work runs, this much silence asks the live model
// for a status. The gaps grow so a long task does not chatter, and start over
// when the owner speaks. The client's typing sounds fill the time in between.
var statusSilenceGaps = [...]time.Duration{10 * time.Second, 20 * time.Second, 40 * time.Second, 80 * time.Second, 160 * time.Second, 320 * time.Second}

// Provider describes the audio format of a live provider.
type Provider struct {
	ID               string
	InputSampleRate  int
	OutputSampleRate int
}

// Settings are what a call was configured with.
type Settings struct {
	Model     string
	Voice     string
	InputMode string
}

// Prompt is what a provider connection starts with.
type Prompt struct {
	Instructions string
	History      []Message
}

// RunningTask is a task this call started that is still running.
type RunningTask struct{ RunID, Request string }

// Delegation is a hand-off registered with the task runtime. Start launches
// its run; the session announces the task before that, so the client never
// sees an outcome ahead of the task it belongs to.
type Delegation struct {
	TaskID string
	RunID  string
	Start  func()
}

// StatusReport is what a call's running work is doing right now.
type StatusReport struct {
	Handle     string
	Facts      string
	LatestStep string
}

// Tasks is the task runtime behind a call: everything the live model hands
// off runs there. Its methods are called with the session's lock held, so
// they must not call back into the session; the runs it starts report through
// the session's exported methods from their own goroutines.
type Tasks interface {
	// Delegate registers a new task, or reports false when there is no request.
	Delegate(handle, request string) (Delegation, bool)
	// RunTool answers the model's task tools (check, update, cancel).
	RunTool(name string, args map[string]any) map[string]any
	Running() []RunningTask
	ActiveRunID() string
	HasRunningWork() bool
	// HasRuns is true while any run of this call has not settled.
	HasRuns() bool
	// StatusReport is nil when there is nothing to report.
	StatusReport() *StatusReport
}

// Store keeps what is said on a call. The app stores a spoken turn like a
// typed one.
type Store interface {
	RecordTurn(role, content string) error
	// RecordTaskReply keeps an outcome the owner never heard, where they will
	// see it.
	RecordTaskReply(runID, content string) error
}

// Texts are the model-facing wordings the session puts on the line. They are
// the app's prompt fragments; the session adds none of its own.
type Texts struct {
	// TaskStarted answers a started hand-off.
	TaskStarted string
	// NoAnswer answers a tool call the runtime could not.
	NoAnswer string
	// EmptyRequest answers a hand-off that named no request.
	EmptyRequest string
	// HandoffWithoutRequest tells the model a GPT-Live hand-off had no request.
	HandoffWithoutRequest string
	// Unanswered asks a model that stayed silent to respond.
	Unanswered string
	// StatusUpdate asks for a status after seconds of silence.
	StatusUpdate func(seconds int, facts string) string
	// LineDropped picks up after a drop.
	LineDropped func(last string) string
}

// Config builds a Session.
type Config struct {
	ID       string
	Provider Provider
	Settings Settings
	Sink     Sink
	// NewAdapter makes the provider connection that reports to the handlers.
	NewAdapter  func(Handlers) Adapter
	BuildPrompt func(ctx context.Context) (Prompt, error)
	Tasks       Tasks
	Store       Store
	Texts       Texts
	// DelegateTool names the function the model starts tasks with (Gemini).
	DelegateTool string
	// ErrorFor words an error for the owner.
	ErrorFor func(error) string
	// OnIdle asks the owner of the session to release it.
	OnIdle func(reason string)
	// OnHangUp tells the owner of the session that the model ended the call.
	OnHangUp func()
	Clock    Clock
	Log      *slog.Logger
}

// owed is what the model was last asked to answer, until it makes a sound.
type owed struct {
	handle    string
	text      string
	retry     bool
	outcome   *Outcome
	lastWords bool
}

// Outcome is a task's result the owner has not heard yet.
type Outcome struct{ RunID, Reply string }

// TellOpts tune Tell.
type TellOpts struct {
	// NoRetry gives up after one silent try instead of asking again.
	NoRetry bool
	// StillTrue is checked once the line is free; false drops the message.
	StillTrue func() bool
	Outcome   *Outcome
}

// Session is one live voice call: the provider connection, who has the line,
// the transcript, tasks handed off, and reconnects. Methods are safe for
// concurrent use.
type Session struct {
	cfg   Config
	clock Clock
	log   *slog.Logger

	mu    sync.Mutex
	floor *Floor

	sink    Sink
	adapter Adapter
	ready   bool
	closed  bool
	state   string

	pendingAudio      [][]byte
	pendingAudioBytes int

	userTurn, assistantTurn, lastUserTurn string
	outputSuppressed                      bool
	playbackEndsAt                        time.Time
	reconnectAttempts                     int
	reconnectedForSilence                 bool

	lastSpokenProgressAt time.Time
	owedReply            *owed
	owedAfterReconnect   *owed
	unheardOutcomes      map[*Outcome]struct{}

	statusTimer       Timer
	statusActive      bool
	statusOwed        bool
	lastStatusStep    string
	lastStatusStepSet bool
	statusStep        int
	statusSilenceFrom time.Time

	speakingTimer Timer
	idleTimer     Timer
	hangUpTimer   Timer
	hangingUp     bool
}

// NewSession builds a session that has not connected yet.
func NewSession(cfg Config) *Session {
	if cfg.Clock == nil {
		cfg.Clock = RealClock{}
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.ErrorFor == nil {
		cfg.ErrorFor = func(err error) string { return err.Error() }
	}
	s := &Session{
		cfg: cfg, clock: cfg.Clock, log: cfg.Log, sink: cfg.Sink,
		state: "connecting", unheardOutcomes: map[*Outcome]struct{}{},
	}
	s.floor = NewFloor(cfg.Clock, &s.mu)
	s.floor.OnReplyMissed = s.recoverSilence
	return s
}

// ID is the session's id.
func (s *Session) ID() string { return s.cfg.ID }

// Attached reports whether a client is listening.
func (s *Session) Attached() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sink != nil
}

// Connect opens the provider connection for the first time.
func (s *Session) Connect(ctx context.Context) error { return s.connect(ctx, false, "") }

func (s *Session) connect(ctx context.Context, reconnected bool, resumeHandle string) error {
	s.mu.Lock()
	s.ready = false
	s.floor.SetConnected(false)
	if reconnected {
		s.setState("reconnecting")
	} else {
		s.setState("connecting")
	}
	// A reattaching client may arrive before the old provider connection was
	// dropped; never leave two billed live sessions open.
	previous := s.adapter
	s.adapter = nil
	s.mu.Unlock()
	if previous != nil {
		previous.Close()
	}

	prompt, err := s.cfg.BuildPrompt(ctx)
	if err != nil {
		return err
	}
	var adapter Adapter
	adapter = s.cfg.NewAdapter(s.handlersFor(func() bool { return s.adapter == adapter }))
	s.mu.Lock()
	s.adapter = adapter
	s.mu.Unlock()
	if err := adapter.Connect(ctx, ConnectParams{Instructions: prompt.Instructions, History: prompt.History, ResumeHandle: resumeHandle}); err != nil {
		return err
	}

	s.mu.Lock()
	if s.adapter != adapter || s.closed {
		s.mu.Unlock()
		adapter.Close()
		return nil
	}
	defer s.mu.Unlock()
	s.ready = true
	s.reconnectAttempts = 0
	s.noteActivity()
	for _, chunk := range s.pendingAudio {
		adapter.AppendAudio(chunk)
	}
	s.pendingAudio, s.pendingAudioBytes = nil, 0
	tasks := []map[string]any{}
	for _, t := range s.cfg.Tasks.Running() {
		tasks = append(tasks, map[string]any{"runId": t.RunID, "request": t.Request})
	}
	var active any
	if id := s.cfg.Tasks.ActiveRunID(); id != "" {
		active = id
	}
	s.emit("session_ready", map[string]any{
		"provider":         s.cfg.Provider.ID,
		"model":            s.cfg.Settings.Model,
		"voice":            s.cfg.Settings.Voice,
		"inputMode":        s.cfg.Settings.InputMode,
		"inputSampleRate":  s.cfg.Provider.InputSampleRate,
		"outputSampleRate": s.cfg.Provider.OutputSampleRate,
		"activeRunId":      active,
		"tasks":            tasks,
		"reconnected":      reconnected,
	})
	s.setState("listening")
	// What arrived while the line was down goes out now.
	s.floor.SetConnected(true)
	s.syncStatusUpdates("")
	return nil
}

// AppendAudio passes the owner's microphone audio to the model; before the
// line is up it is kept, up to a few seconds.
func (s *Session) AppendAudio(pcm []byte) {
	if len(pcm) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ready {
		s.adapter.AppendAudio(pcm)
		return
	}
	s.pendingAudio = append(s.pendingAudio, pcm)
	s.pendingAudioBytes += len(pcm)
	for s.pendingAudioBytes > maxPendingAudioBytes {
		s.pendingAudioBytes -= len(s.pendingAudio[0])
		s.pendingAudio = s.pendingAudio[1:]
	}
}

// StartInput: pressing push-to-talk talks over the assistant, like picking up
// mid-sentence.
func (s *Session) StartInput() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.ready {
		return
	}
	if s.cfg.Settings.InputMode == "ptt" {
		s.interruptOutput()
		s.floor.SetInputHeld(true)
	}
	s.adapter.StartInput()
}

// EndInput ends the owner's turn.
func (s *Session) EndInput() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.ready {
		return
	}
	if s.cfg.Settings.InputMode == "ptt" {
		s.floor.SetInputHeld(false)
	}
	s.adapter.EndInput()
}

// InterruptOutput: the owner asked the assistant to stop talking; what is
// still coming is dropped until they speak again.
func (s *Session) InterruptOutput() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.interruptOutput()
}

func (s *Session) interruptOutput() {
	s.outputSuppressed = true
	s.emit("interrupted", nil)
	s.flushAssistantTurn()
	s.setState("listening")
	s.floor.OwnerInterrupted()
}

// Say has the model tell the owner something once the line is free.
func (s *Session) Say(text string) { s.Tell("", text, TellOpts{}) }

// Tell: something the owner must hear. It is news, so speech the owner
// stopped earlier does not silence it.
func (s *Session) Tell(handle, text string, o TellOpts) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tell(handle, text, o)
}

func (s *Session) tell(handle, text string, o TellOpts) {
	// Until it has been heard, an outcome also has to survive the call ending.
	if o.Outcome != nil {
		s.unheardOutcomes[o.Outcome] = struct{}{}
	}
	s.floor.WhenFree(func() {
		// It may have waited for the line, and what it says may no longer hold.
		if o.StillTrue != nil && !o.StillTrue() {
			return
		}
		s.outputSuppressed = false
		s.adapter.Say(text, handle)
		s.owedReply = &owed{handle: handle, text: text, retry: !o.NoRetry, outcome: o.Outcome}
		s.floor.ExpectReply(true)
	})
}

// Inform gives the model context it uses when it matters, without saying it
// now.
func (s *Session) Inform(handle, text string, stillTrue func() bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inform(handle, text, stillTrue)
}

func (s *Session) inform(handle, text string, stillTrue func() bool) {
	s.floor.WhenFree(func() {
		if stillTrue != nil && !stillTrue() {
			return
		}
		s.adapter.Note(text, handle)
	})
}

// TellOutcome: an outcome replaces a status the model has not started saying.
func (s *Session) TellOutcome(handle, text, runID, reply string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tellOutcome(handle, text, runID, reply)
}

func (s *Session) tellOutcome(handle, text, runID, reply string) {
	if s.statusOwed {
		s.floor.DropUnstartedReply()
	}
	s.statusOwed = false
	s.tell(handle, text, TellOpts{Outcome: &Outcome{RunID: runID, Reply: reply}})
}

// OfferProgress: a progress update fills a silence; while the owner and the
// assistant are talking, or shortly after the last one, it only joins the
// context.
func (s *Session) OfferProgress(handle, text string, stillTrue func() bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.clock.Now()
	if s.floor.Quiet() && now.Sub(s.lastSpokenProgressAt) >= spokenProgressGap {
		s.lastSpokenProgressAt = now
		s.tell(handle, text, TellOpts{StillTrue: stillTrue, NoRetry: true})
		return
	}
	s.inform(handle, text, stillTrue)
}

// DeliverTaskOutcome: a task finished. A listening owner hears it; otherwise
// the reply goes to the chat.
func (s *Session) DeliverTaskOutcome(handle, runID, outcome, reply string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sink != nil {
		if reply == "" {
			reply = outcome
		}
		s.tellOutcome(handle, outcome, runID, reply)
		return
	}
	if reply != "" {
		if err := s.cfg.Store.RecordTaskReply(runID, reply); err != nil {
			s.log.Warn("Failed to store a task reply", "error", err)
		}
	}
	s.releaseIfIdle()
}

// PublishTask tells the client a task started or settled.
func (s *Session) PublishTask(runID, status, request string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.publishTask(runID, status, request)
}

func (s *Session) publishTask(runID, status, request string) {
	s.noteActivity()
	s.syncStatusUpdates(status)
	payload := map[string]any{"runId": runID, "status": status}
	if request != "" {
		payload["request"] = request
	}
	s.emit("task", payload)
}

// ReleaseIfIdle lets the owner release a session nobody listens to and no
// task keeps alive.
func (s *Session) ReleaseIfIdle() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.releaseIfIdle()
}

func (s *Session) releaseIfIdle() {
	if s.sink == nil && !s.cfg.Tasks.HasRunningWork() && s.cfg.OnIdle != nil {
		go s.cfg.OnIdle("released")
	}
}

// Attach reconnects a returning client to the call.
func (s *Session) Attach(ctx context.Context, sink Sink) error {
	s.mu.Lock()
	s.sink = sink
	s.mu.Unlock()
	if err := s.connect(ctx, true, ""); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resumeOwedReply()
	return nil
}

// Detach: the client went away. Running hand-offs keep going and the session
// stays reattachable until they finish.
func (s *Session) Detach() {
	s.mu.Lock()
	if s.owedAfterReconnect == nil {
		s.owedAfterReconnect = s.owedReply
	}
	s.owedReply = nil
	s.sink = nil
	s.mu.Unlock()
	s.closeAdapter()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.flushTurns()
	s.keepUnheardOutcomes()
	s.releaseIfIdle()
}

// keepUnheardOutcomes: the owner never heard these, so they go where they will
// see them.
func (s *Session) keepUnheardOutcomes() {
	for o := range s.unheardOutcomes {
		if err := s.cfg.Store.RecordTaskReply(o.RunID, o.Reply); err != nil {
			s.log.Warn("Failed to store a task outcome", "error", err)
		}
	}
	clear(s.unheardOutcomes)
}

// Close ends the call. The caller aborts running tasks first when it wants to.
func (s *Session) Close(reason string) {
	s.mu.Lock()
	s.flushTurns()
	s.keepUnheardOutcomes()
	s.emit("state", map[string]any{"state": "closed", "reason": reason})
	s.closed = true
	s.sink = nil
	s.floor.Close()
	s.mu.Unlock()
	s.closeAdapter()
}

func (s *Session) noteActivity() {
	stop(s.idleTimer)
	s.idleTimer = s.clock.AfterFunc(idleHangup, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.closed || s.sink == nil {
			return
		}
		if s.cfg.Tasks.HasRunningWork() {
			s.noteActivity()
			return
		}
		s.log.Info("Hanging up idle live voice call", "sessionId", s.cfg.ID)
		if s.cfg.OnIdle != nil {
			go s.cfg.OnIdle("idle_timeout")
		}
	})
}

func (s *Session) closeAdapter() {
	s.mu.Lock()
	s.ready = false
	s.floor.SetConnected(false)
	s.stopStatusUpdates()
	stop(s.speakingTimer)
	stop(s.idleTimer)
	stop(s.hangUpTimer)
	adapter := s.adapter
	s.adapter = nil
	s.mu.Unlock()
	if adapter != nil {
		adapter.Close()
	}
}

// handlersFor wraps the session's reactions so events of a replaced
// connection are ignored and a failing handler cannot take the call down.
func (s *Session) handlersFor(isCurrent func() bool) Handlers {
	guard := func(fn func()) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if !isCurrent() || s.closed {
			return
		}
		defer func() {
			if r := recover(); r != nil {
				s.log.Warn("Live event handling failed", "error", fmt.Sprint(r))
			}
		}()
		fn()
	}
	return Handlers{
		OnAudio:            func(pcm []byte) { guard(func() { s.handleAudio(pcm) }) },
		OnInputTranscript:  func(text string) { guard(func() { s.handleInputTranscript(text) }) },
		OnOutputTranscript: func(text string) { guard(func() { s.handleOutputTranscript(text) }) },
		// The owner talked over the model: their turn has the line.
		OnInterrupted: func() {
			guard(func() {
				s.playbackEndsAt = time.Time{}
				s.emit("interrupted", nil)
				s.flushAssistantTurn()
				s.setState("listening")
				s.floor.OwnerInterrupted()
			})
		},
		OnTurnComplete: func(idle bool) {
			guard(func() {
				s.flushAssistantTurn()
				s.floor.TurnEnded()
				if idle {
					s.floor.ModelIdle()
				} else {
					s.floor.ModelWorking()
				}
			})
		},
		OnToolCall: func(call ToolCall) (any, bool) {
			var response any = map[string]any{"error": s.cfg.Texts.NoAnswer}
			guard(func() { response = s.handleToolCall(call) })
			return response, true
		},
		OnHangUp: func() {
			guard(func() {
				s.log.Info("Live model hung up the call", "sessionId", s.cfg.ID)
				s.hangingUp = true
				s.scheduleHangUp()
			})
		},
		OnDelegation: func(handle string) string {
			guard(func() { s.handleDelegation(handle, "") })
			return ""
		},
		OnError: func(err error) {
			guard(func() {
				s.log.Warn("Live provider error", "sessionId", s.cfg.ID, "error", err.Error())
				s.emit("error", map[string]any{"error": s.cfg.ErrorFor(err), "recoverable": true})
			})
		},
		OnClosed: func(reason, resumeHandle string) {
			guard(func() { go s.reconnect(reason, resumeHandle) })
		},
	}
}

// handleToolCall answers a Gemini tool call at once, and the model speaks
// next. Starting work returns with the task's real state while the task runs
// in the background: Gemini fills the silence of a pending call by inventing
// an outcome.
func (s *Session) handleToolCall(call ToolCall) any {
	if call.Name != s.cfg.DelegateTool {
		s.expectToolReply("")
		if answer := s.cfg.Tasks.RunTool(call.Name, call.Args); answer != nil {
			return answer
		}
		return map[string]any{"error": s.cfg.Texts.NoAnswer}
	}
	request, _ := call.Args["request"].(string)
	request = TrimJS(request)
	if request == "" {
		return map[string]any{"error": s.cfg.Texts.EmptyRequest}
	}
	s.expectToolReply(call.ID)
	taskID := s.handleDelegation(call.ID, request)
	if taskID == "" {
		return map[string]any{"error": s.cfg.Texts.NoAnswer}
	}
	return map[string]any{"result": s.cfg.Texts.TaskStarted, "task_id": taskID}
}

// expectToolReply: a live model sometimes takes a tool result or a message
// and says nothing.
func (s *Session) expectToolReply(handle string) {
	s.owedReply = &owed{handle: handle, text: s.cfg.Texts.Unanswered, retry: true}
	s.floor.ExpectReply(false)
}

// recoverSilence is one more try, in the model's own words. Silent again on an
// open line, the connection is stuck rather than the model choosing quiet: a
// fresh one gets what it owes, once until the model speaks again. Whatever is
// still owed after that is in the chat, so the owner can find it.
func (s *Session) recoverSilence() {
	o := s.owedReply
	s.owedReply = nil
	s.statusOwed = false
	if o == nil {
		return
	}
	if o.retry {
		s.log.Info("Live model stayed silent; asking again", "sessionId", s.cfg.ID)
		s.tell(o.handle, o.text, TellOpts{NoRetry: true, Outcome: o.outcome})
		return
	}
	if s.reconnectedForSilence {
		return
	}
	s.reconnectedForSilence = true
	again := *o
	again.retry = true
	s.owedReply = &again
	go s.reconnect("model_unresponsive", "")
}

func (s *Session) syncStatusUpdates(taskStatus string) {
	if taskStatus == "running" {
		s.statusStep = 0
		s.statusSilenceFrom = s.clock.Now()
	}
	if !s.cfg.Tasks.HasRuns() {
		s.stopStatusUpdates()
		return
	}
	if s.statusActive {
		return
	}
	s.lastStatusStepSet = false
	s.statusActive = true
	s.armStatusTick()
}

func (s *Session) armStatusTick() {
	s.statusTimer = s.clock.AfterFunc(statusTick, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if !s.statusActive {
			return
		}
		s.speakStatusIfDue()
		if s.statusActive {
			s.armStatusTick()
		}
	})
}

func (s *Session) stopStatusUpdates() {
	s.statusActive = false
	stop(s.statusTimer)
	s.statusTimer = nil
}

// speakStatusIfDue asks the model that is already on the line, so a status
// costs no extra run and works while the task sits inside one long tool. An
// owner who stopped the speech is not talked back into until they speak.
func (s *Session) speakStatusIfDue() {
	if s.closed || s.sink == nil || !s.ready || s.hangingUp || s.outputSuppressed || !s.floor.Free() {
		return
	}
	silentFor := min(s.floor.SilentFor(), s.clock.Now().Sub(s.statusSilenceFrom))
	if silentFor < statusSilenceGaps[0] {
		return
	}
	report := s.cfg.Tasks.StatusReport()
	if report == nil {
		return
	}
	// Every status waits longer than the last, so a long task is not narrated
	// step by step; with nothing new to say it waits one step longer still. The
	// owner speaking starts the sequence over.
	last := len(statusSilenceGaps) - 1
	gap := statusSilenceGaps[min(s.statusStep+1, last)]
	if !s.lastStatusStepSet || report.LatestStep != s.lastStatusStep {
		gap = statusSilenceGaps[min(s.statusStep, last)]
	}
	if silentFor < gap {
		return
	}
	s.statusStep++
	s.lastStatusStep, s.lastStatusStepSet = report.LatestStep, true
	s.lastSpokenProgressAt = s.clock.Now()
	s.statusOwed = true
	seconds := int(math.Floor(silentFor.Seconds() + 0.5))
	s.tell(report.Handle, s.cfg.Texts.StatusUpdate(seconds, report.Facts), TellOpts{NoRetry: true})
}

func (s *Session) handleAudio(pcm []byte) {
	s.reconnectedForSilence = false
	if s.owedReply != nil && s.owedReply.outcome != nil {
		delete(s.unheardOutcomes, s.owedReply.outcome)
	}
	s.owedReply = nil
	s.statusOwed = false
	s.noteActivity()
	stop(s.speakingTimer)
	s.speakingTimer = s.clock.AfterFunc(speakingIdle, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.setState("listening")
		s.floor.ModelSilent()
		// GPT-Live reports no turn ends; its speech ending is the closest.
		if s.adapter != nil && !s.adapter.ReportsTurnEnds() {
			s.floor.TurnEnded()
		}
	})
	if s.outputSuppressed {
		s.floor.DroppedSpeech()
		return
	}
	s.emit("audio", map[string]any{"audioBase64": audiox.EncodeBase64(pcm)})
	play := time.Duration(audiox.DurationMs(len(pcm), s.cfg.Provider.OutputSampleRate) * float64(time.Millisecond))
	s.playbackEndsAt = laterOf(s.clock.Now(), s.playbackEndsAt).Add(play)
	s.floor.ModelSpeaking(s.playbackEndsAt)
	if s.hangingUp {
		s.scheduleHangUp()
	}
	s.setState("speaking")
}

func (s *Session) handleInputTranscript(text string) {
	if text == "" {
		return
	}
	s.noteActivity()
	s.floor.OwnerSpoke()
	s.statusStep = 0
	s.outputSuppressed = false
	s.flushAssistantTurn()
	s.userTurn = AppendFragment(s.userTurn, text)
	s.emit("transcript", map[string]any{"role": "user", "content": s.userTurn, "final": false})
}

func (s *Session) handleOutputTranscript(text string) {
	if text == "" || s.outputSuppressed {
		return
	}
	s.flushUserTurn()
	s.assistantTurn = AppendFragment(s.assistantTurn, text)
	s.emit("transcript", map[string]any{"role": "assistant", "content": s.assistantTurn, "final": false})
}

// scheduleHangUp: speech still arriving after the hang-up pushes it back, so
// the goodbye is heard to the end. The client hears the line drop here; the
// owner of the session then ends it like an owner hang-up, keeping running
// tasks going.
func (s *Session) scheduleHangUp() {
	stop(s.hangUpTimer)
	delay := max(0, s.playbackEndsAt.Sub(s.clock.Now())) + hangupGrace
	s.hangUpTimer = s.clock.AfterFunc(delay, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.hangingUp = false
		if s.closed || s.sink == nil {
			return
		}
		s.emit("state", map[string]any{"state": "closed", "reason": "agent_hung_up"})
		s.sink = nil
		if s.cfg.OnHangUp != nil {
			go s.cfg.OnHangUp()
		}
	})
}

// handleDelegation returns the new task's id for a model that names its
// request. GPT-Live hands off without task text: the request is the owner's
// latest turn, and the run reads the rest of the conversation from its history.
func (s *Session) handleDelegation(handle, request string) string {
	if request != "" {
		return s.delegate(handle, request)
	}
	s.clock.AfterFunc(delegationSettle, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.closed || !s.ready {
			return
		}
		latest := s.userTurn
		if latest == "" {
			latest = s.lastUserTurn
		}
		if s.delegate(handle, latest) == "" {
			s.tell(handle, s.cfg.Texts.HandoffWithoutRequest, TellOpts{})
		}
	})
	return ""
}

func (s *Session) delegate(handle, request string) string {
	d, ok := s.cfg.Tasks.Delegate(handle, request)
	if !ok {
		return ""
	}
	s.publishTask(d.RunID, "running", TrimJS(request))
	d.Start()
	return d.TaskID
}

func (s *Session) reconnect(reason, resumeHandle string) {
	for {
		s.mu.Lock()
		// A drop loses what the model was about to say, so what the owner was
		// waiting for is asked again on the new connection.
		if s.owedAfterReconnect == nil {
			s.owedAfterReconnect = s.owedReply
			if s.owedAfterReconnect == nil && s.floor.OwnerWaiting() {
				s.owedAfterReconnect = &owed{lastWords: true, retry: true}
			}
		}
		s.owedReply = nil
		s.ready = false
		s.floor.SetConnected(false)
		s.flushTurns()
		if s.sink == nil || s.closed {
			s.mu.Unlock()
			return
		}
		s.reconnectAttempts++
		if s.reconnectAttempts > maxReconnectAttempts {
			s.emit("error", map[string]any{
				"error":       s.cfg.ErrorFor(fmt.Errorf("The live voice connection ended (%s).", reason)),
				"recoverable": false,
			})
			s.emit("state", map[string]any{"state": "closed", "reason": reason})
			s.mu.Unlock()
			s.Detach()
			return
		}
		attempt := s.reconnectAttempts
		s.mu.Unlock()
		s.log.Info("Reconnecting live voice", "sessionId", s.cfg.ID, "reason", reason, "attempt", attempt)
		err := s.connect(context.Background(), true, resumeHandle)
		if err == nil {
			s.mu.Lock()
			s.resumeOwedReply()
			s.mu.Unlock()
			return
		}
		reason, resumeHandle = err.Error(), ""
	}
}

func (s *Session) resumeOwedReply() {
	o := s.owedAfterReconnect
	s.owedAfterReconnect = nil
	if o == nil {
		return
	}
	if !o.lastWords {
		s.tell(o.handle, o.text, TellOpts{NoRetry: !o.retry, Outcome: o.outcome})
	} else if s.lastUserTurn != "" {
		s.tell("", s.cfg.Texts.LineDropped(s.lastUserTurn), TellOpts{})
	}
}

func (s *Session) flushUserTurn() {
	content := TrimJS(s.userTurn)
	s.userTurn = ""
	if content == "" {
		return
	}
	s.lastUserTurn = content
	s.persistTurn("user", content)
}

func (s *Session) flushAssistantTurn() {
	content := TrimJS(s.assistantTurn)
	s.assistantTurn = ""
	if content == "" {
		return
	}
	s.persistTurn("assistant", content)
}

func (s *Session) flushTurns() {
	s.flushUserTurn()
	s.flushAssistantTurn()
}

func (s *Session) persistTurn(role, content string) {
	s.emit("transcript", map[string]any{"role": role, "content": content, "final": true})
	if err := s.cfg.Store.RecordTurn(role, content); err != nil {
		s.log.Warn("Failed to store voice turn", "error", err)
	}
}

func (s *Session) setState(state string) {
	if s.state == state {
		return
	}
	s.state = state
	s.emit("state", map[string]any{"state": state})
}

func (s *Session) emit(kind string, payload map[string]any) {
	if s.sink == nil {
		return
	}
	out := make(map[string]any, len(payload)+1)
	out["sessionId"] = s.cfg.ID
	for k, v := range payload {
		out[k] = v
	}
	s.sink.Send(kind, out)
}
