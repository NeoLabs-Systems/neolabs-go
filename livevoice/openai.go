package livevoice

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/NeoLabs-Systems/neolabs-go/audiox"
	"github.com/NeoLabs-Systems/neolabs-go/wsclient"
)

const (
	openAISampleRate = 24000
	// GPT-Live advances its conversation timeline with the audio it receives, so
	// a push-to-talk release or a stalled microphone stream is filled with
	// silence to let it hear the end of the turn. The gap threshold stays well
	// above any microphone chunk interval so real speech is never split.
	silenceTick = 100 * time.Millisecond
	silenceGap  = 500 * time.Millisecond
	// The API caps startup history at 128 messages and 8,192 tokens.
	maxStartupHistoryChars = 24000
	maxStartupMessages     = 128
	maxAppendUnits         = 1800
)

// OpenAIConfig configures a GPT-Live connection.
type OpenAIConfig struct {
	APIKey     string
	BaseURL    string
	Model      string
	Voice      string
	HTTPClient *http.Client
	Handlers   Handlers
}

// OpenAI is a GPT-Live (/live/sessions) connection.
type OpenAI struct {
	cfg OpenAIConfig

	mu          sync.Mutex
	conn        *wsclient.Conn
	cancel      context.CancelFunc
	closing     bool
	lastAudioAt time.Time
	inputEnded  bool
	delegations map[string]struct{}
	start       chan error // nil once the session started
	stopSilence chan struct{}
}

// NewOpenAI builds an unconnected adapter.
func NewOpenAI(cfg OpenAIConfig) *OpenAI {
	return &OpenAI{cfg: cfg, delegations: map[string]struct{}{}}
}

// ReportsTurnEnds: GPT-Live has no event that marks the end of a model turn.
func (o *OpenAI) ReportsTurnEnds() bool { return false }

func openAISocketURL(baseURL string) string {
	base := strings.TrimSpace(baseURL)
	if base == "" {
		base = "https://api.openai.com/v1"
	}
	lower := strings.ToLower(base)
	switch {
	case strings.HasPrefix(lower, "https:"):
		base = "wss:" + base[len("https:"):]
	case strings.HasPrefix(lower, "http:"):
		base = "ws:" + base[len("http:"):]
	}
	return strings.TrimSuffix(base, "/") + "/live/sessions"
}

func startupInput(history []Message) []any {
	budget := maxStartupHistoryChars
	var items []any
	for i := len(history) - 1; i >= 0; i-- {
		m := history[i]
		budget -= utf16Len(m.Text)
		if budget < 0 || len(items) >= maxStartupMessages {
			break
		}
		kind := "input_text"
		if m.Role == "assistant" {
			kind = "output_text"
		}
		item := map[string]any{
			"type": "message", "role": m.Role,
			"content": []any{map[string]any{"type": kind, "text": m.Text}},
		}
		items = append([]any{item}, items...)
	}
	return items
}

// Connect opens the socket and waits for the session to start.
func (o *OpenAI) Connect(ctx context.Context, p ConnectParams) error {
	if o.cfg.APIKey == "" {
		return errors.New("GPT-Live is selected but no OpenAI API key is configured.")
	}
	parent := ctx
	ctx, cancelOpen := context.WithTimeout(ctx, openTimeout)
	defer cancelOpen()
	conn, err := wsclient.Dial(ctx, openAISocketURL(o.cfg.BaseURL), wsclient.Options{
		Header:    http.Header{"Authorization": {"Bearer " + o.cfg.APIKey}},
		ReadLimit: readLimit, HTTPClient: o.cfg.HTTPClient,
	})
	if err != nil {
		return err
	}
	readCtx, cancel := context.WithCancel(context.Background())
	o.mu.Lock()
	o.conn, o.cancel = conn, cancel
	o.start = make(chan error, 1)
	start := o.start
	o.mu.Unlock()

	go o.readLoop(readCtx, conn)
	items := startupInput(p.History)
	if items == nil {
		items = []any{}
	}
	o.send(map[string]any{"type": "session.start", "session": map[string]any{
		"model":        o.cfg.Model,
		"instructions": p.Instructions,
		"input":        items,
		"audio":        map[string]any{"output": map[string]any{"voice": o.cfg.Voice}},
		"delegation":   map[string]any{"type": "client"},
	}})

	select {
	case err := <-start:
		if err != nil {
			return err
		}
	case <-ctx.Done():
		o.terminate()
		if parent.Err() != nil {
			return parent.Err()
		}
		return errors.New("GPT-Live did not start the session in time.")
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closing {
		return nil
	}
	o.stopSilence = make(chan struct{})
	go o.fillSilence(o.stopSilence)
	return nil
}

// AppendAudio streams 24 kHz mono PCM to the model.
func (o *OpenAI) AppendAudio(pcm []byte) {
	o.mu.Lock()
	o.lastAudioAt = time.Now()
	o.inputEnded = false
	o.mu.Unlock()
	o.send(map[string]any{"type": "session.input_audio.append", "audio": audiox.EncodeBase64(pcm)})
}

// StartInput marks the start of a push-to-talk turn.
func (o *OpenAI) StartInput() {
	o.mu.Lock()
	o.inputEnded = false
	o.mu.Unlock()
}

// EndInput marks the end of a turn; silence fills in so the model hears it.
func (o *OpenAI) EndInput() {
	o.mu.Lock()
	o.inputEnded = true
	o.mu.Unlock()
}

// Say: commentary is for the model to say.
func (o *OpenAI) Say(text, handle string) { o.appendText("session.commentary.append", handle, text) }

// Note: thinking is context the model keeps quiet about. Both name the
// hand-off they answer; one from an earlier connection is unknown to this
// session, so its updates go in as general context.
func (o *OpenAI) Note(text, handle string) { o.appendText("session.thinking.append", handle, text) }

func (o *OpenAI) appendText(kind, handle, text string) {
	o.mu.Lock()
	_, known := o.delegations[handle]
	o.mu.Unlock()
	var id any
	if known {
		id = handle
	}
	for _, part := range SplitForAppend(text, maxAppendUnits) {
		o.send(map[string]any{"type": kind, "delegation_id": id, "content": part})
	}
}

// Close ends the session with a goodbye.
func (o *OpenAI) Close() {
	o.mu.Lock()
	o.closing = true
	conn, stopped := o.conn, o.stopSilence
	o.stopSilence = nil
	started := o.start == nil
	o.mu.Unlock()
	if stopped != nil {
		close(stopped)
	}
	if conn == nil {
		return
	}
	if !started {
		o.terminate()
		return
	}
	o.send(map[string]any{"type": "session.close"})
	time.AfterFunc(time.Second, func() { _ = conn.Close(wsclient.CloseNormal, "") })
}

func (o *OpenAI) terminate() {
	o.mu.Lock()
	conn, cancel := o.conn, o.cancel
	o.mu.Unlock()
	if conn != nil {
		_ = conn.CloseNow()
	}
	if cancel != nil {
		cancel()
	}
}

func (o *OpenAI) fillSilence(stopped <-chan struct{}) {
	ticker := time.NewTicker(silenceTick)
	defer ticker.Stop()
	frame := audiox.EncodeBase64(audiox.Silence(openAISampleRate, int(silenceTick/time.Millisecond)))
	for {
		select {
		case <-stopped:
			return
		case <-ticker.C:
		}
		o.mu.Lock()
		due := o.inputEnded || time.Since(o.lastAudioAt) >= silenceGap
		o.mu.Unlock()
		if due {
			o.send(map[string]any{"type": "session.input_audio.append", "audio": frame})
		}
	}
}

func (o *OpenAI) send(event map[string]any) bool {
	o.mu.Lock()
	conn := o.conn
	o.mu.Unlock()
	if conn == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), writeTimeout)
	defer cancel()
	return conn.WriteJSON(ctx, event) == nil
}

func (o *OpenAI) readLoop(ctx context.Context, conn *wsclient.Conn) {
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			o.handleClose("connection_lost")
			return
		}
		o.handleEvent(parseObject(data))
	}
}

func (o *OpenAI) handleEvent(event map[string]any) {
	h := o.cfg.Handlers
	switch str(event, "type") {
	case "session.started":
		o.finishStart(nil)
	case "session.output_audio.delta":
		if delta := str(event, "delta"); delta != "" {
			h.OnAudio(audiox.DecodeBase64(delta))
		}
	case "session.input_transcript.delta":
		h.OnInputTranscript(str(event, "delta"))
	case "session.output_transcript.delta":
		h.OnOutputTranscript(str(event, "delta"))
	case "session.delegation.created":
		d := obj(event, "delegation")
		if id := str(d, "id"); str(d, "target") == "client" && id != "" {
			o.mu.Lock()
			o.delegations[id] = struct{}{}
			o.mu.Unlock()
			h.OnDelegation(id)
		}
	case "session.closed":
		reason := str(event, "reason")
		if reason == "" {
			reason = "closed"
		}
		o.handleClose(reason)
	case "error":
		e := obj(event, "error")
		message := str(e, "message")
		if message == "" {
			message = "GPT-Live reported an error."
		}
		if o.finishStart(errors.New(message)) {
			return
		}
		h.OnError(errors.New(message))
	}
}

func (o *OpenAI) finishStart(err error) bool {
	o.mu.Lock()
	start := o.start
	o.start = nil
	o.mu.Unlock()
	if start == nil {
		return false
	}
	start <- err
	return true
}

// handleClose: a connection that never started is reported through Connect's
// error, not as a dropped session.
func (o *OpenAI) handleClose(reason string) {
	o.mu.Lock()
	stopped := o.stopSilence
	o.stopSilence = nil
	o.mu.Unlock()
	if stopped != nil {
		close(stopped)
	}
	neverStarted := o.finishStart(fmt.Errorf("GPT-Live closed before the session started (%s).", reason))
	o.mu.Lock()
	if o.closing || neverStarted {
		o.mu.Unlock()
		return
	}
	o.closing = true
	o.mu.Unlock()
	o.cfg.Handlers.OnClosed(reason, "")
}
