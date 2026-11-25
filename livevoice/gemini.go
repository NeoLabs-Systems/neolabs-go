package livevoice

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/NeoLabs-Systems/neolabs-go/audiox"
	"github.com/NeoLabs-Systems/neolabs-go/wsclient"
)

const (
	geminiOrigin   = "https://generativelanguage.googleapis.com"
	geminiLivePath = "/ws/google.ai.generativelanguage.v1beta.GenerativeService.BidiGenerateContent"
	openTimeout    = 15 * time.Second
	readLimit      = 16 << 20
	writeTimeout   = 10 * time.Second
)

// GeminiConfig configures a Gemini Live connection.
type GeminiConfig struct {
	APIKey  string
	BaseURL string
	Model   string
	// Thinking models take a thinking level, and some refuse a session without
	// one; low keeps the first words of an answer quick. Others reject it.
	Thinking bool
	Voice    string
	// InputMode "ptt" marks each turn explicitly; Gemini's own speech detection
	// needs trailing silence a released button never sends.
	InputMode string
	// FunctionDeclarations are the tools the model may call, in Gemini's schema.
	FunctionDeclarations []any
	// HangUpTool names the tool that ends the call.
	HangUpTool string
	HTTPClient *http.Client
	Handlers   Handlers
}

// Gemini is a Gemini Live (BidiGenerateContent) connection.
type Gemini struct {
	cfg         GeminiConfig
	manualTurns bool

	mu           sync.Mutex
	conn         *wsclient.Conn
	cancel       context.CancelFunc
	closing      bool
	resumeHandle string
	setup        chan error // nil once setup finished
}

// NewGemini builds an unconnected adapter.
func NewGemini(cfg GeminiConfig) *Gemini {
	return &Gemini{cfg: cfg, manualTurns: cfg.InputMode == "ptt"}
}

// ReportsTurnEnds: Gemini marks the end of each model turn.
func (g *Gemini) ReportsTurnEnds() bool { return true }

func geminiSocketURL(baseURL, apiKey string) (string, error) {
	if strings.TrimSpace(baseURL) == "" {
		baseURL = geminiOrigin
	}
	u, err := url.Parse(baseURL)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("Invalid URL: %s", baseURL)
	}
	scheme := "ws"
	if u.Scheme == "https" || u.Scheme == "wss" {
		scheme = "wss"
	}
	return fmt.Sprintf("%s://%s%s?key=%s", scheme, u.Host, geminiLivePath, url.QueryEscape(apiKey)), nil
}

// withHistory: Gemini Live has no startup history slot, so the recent thread
// rides along in the system instruction.
func withHistory(instructions string, history []Message) string {
	if len(history) == 0 {
		return instructions
	}
	lines := make([]string, len(history))
	for i, m := range history {
		who := "Owner"
		if m.Role == "assistant" {
			who = "You"
		}
		lines[i] = who + ": " + m.Text
	}
	return instructions + "\n\nRecent conversation:\n" + strings.Join(lines, "\n")
}

// Connect opens the socket and waits for the provider's setup to complete.
func (g *Gemini) Connect(ctx context.Context, p ConnectParams) error {
	if g.cfg.APIKey == "" {
		return errors.New("Gemini Live is selected but no Google AI API key is configured.")
	}
	target, err := geminiSocketURL(g.cfg.BaseURL, g.cfg.APIKey)
	if err != nil {
		return err
	}
	parent := ctx
	ctx, cancelOpen := context.WithTimeout(ctx, openTimeout)
	defer cancelOpen()
	conn, err := wsclient.Dial(ctx, target, wsclient.Options{ReadLimit: readLimit, HTTPClient: g.cfg.HTTPClient})
	if err != nil {
		return sanitizeDialError(err, g.cfg.APIKey)
	}
	readCtx, cancel := context.WithCancel(context.Background())
	g.mu.Lock()
	g.conn, g.cancel = conn, cancel
	g.setup = make(chan error, 1)
	setup := g.setup
	g.mu.Unlock()

	go g.readLoop(readCtx, conn)
	g.send(g.setupMessage(p))

	select {
	case err := <-setup:
		return err
	case <-ctx.Done():
		g.terminate()
		if parent.Err() != nil {
			return parent.Err()
		}
		return errors.New("Gemini Live did not complete setup in time.")
	}
}

func (g *Gemini) setupMessage(p ConnectParams) map[string]any {
	generation := map[string]any{
		"responseModalities": []string{"AUDIO"},
		"speechConfig": map[string]any{"voiceConfig": map[string]any{
			"prebuiltVoiceConfig": map[string]any{"voiceName": g.cfg.Voice},
		}},
	}
	if g.cfg.Thinking {
		generation["thinkingConfig"] = map[string]any{"thinkingLevel": "low"}
	}
	// Hands-free, only clear speech counts as the owner starting to talk, so
	// the call's own audio coming back through the microphone does not cut the
	// model off.
	detection := map[string]any{"startOfSpeechSensitivity": "START_SENSITIVITY_LOW"}
	if g.manualTurns {
		detection = map[string]any{"disabled": true}
	}
	resumption := map[string]any{}
	if p.ResumeHandle != "" {
		resumption["handle"] = p.ResumeHandle
	}
	return map[string]any{"setup": map[string]any{
		"model":                    "models/" + g.cfg.Model,
		"generationConfig":         generation,
		"systemInstruction":        map[string]any{"parts": []any{map[string]any{"text": withHistory(p.Instructions, p.History)}}},
		"tools":                    []any{map[string]any{"functionDeclarations": g.cfg.FunctionDeclarations}},
		"inputAudioTranscription":  map[string]any{},
		"outputAudioTranscription": map[string]any{},
		"realtimeInputConfig":      map[string]any{"automaticActivityDetection": detection},
		"sessionResumption":        resumption,
		"contextWindowCompression": map[string]any{"slidingWindow": map[string]any{}},
	}}
}

// AppendAudio streams 16 kHz mono PCM to the model.
func (g *Gemini) AppendAudio(pcm []byte) {
	g.send(map[string]any{"realtimeInput": map[string]any{
		"audio": map[string]any{"mimeType": "audio/pcm;rate=16000", "data": audiox.EncodeBase64(pcm)},
	}})
}

// StartInput marks the start of a push-to-talk turn.
func (g *Gemini) StartInput() {
	if g.manualTurns {
		g.send(map[string]any{"realtimeInput": map[string]any{"activityStart": map[string]any{}}})
	}
}

// EndInput marks the end of a turn.
func (g *Gemini) EndInput() {
	if g.manualTurns {
		g.send(map[string]any{"realtimeInput": map[string]any{"activityEnd": map[string]any{}}})
		return
	}
	g.send(map[string]any{"realtimeInput": map[string]any{"audioStreamEnd": true}})
}

// Say starts a new turn, which cuts off the answer Gemini is generating; the
// session only calls it while the line is free.
func (g *Gemini) Say(text, _ string) {
	g.clientText("Tell the owner now, in your own words:\n"+text, true)
}

// Note adds context without asking for an answer.
func (g *Gemini) Note(text, _ string) {
	g.clientText("Context update (do not read aloud unless it becomes relevant):\n"+text, false)
}

func (g *Gemini) clientText(text string, turnComplete bool) {
	g.send(map[string]any{"clientContent": map[string]any{
		"turns":        []any{map[string]any{"role": "user", "parts": []any{map[string]any{"text": text}}}},
		"turnComplete": turnComplete,
	}})
}

// Close ends the call without reporting it as a dropped connection.
func (g *Gemini) Close() {
	g.mu.Lock()
	g.closing = true
	conn := g.conn
	g.mu.Unlock()
	if conn != nil {
		go func() { _ = conn.Close(wsclient.CloseNormal, "") }()
	}
}

func (g *Gemini) terminate() {
	g.mu.Lock()
	conn, cancel := g.conn, g.cancel
	g.mu.Unlock()
	if conn != nil {
		_ = conn.CloseNow()
	}
	if cancel != nil {
		cancel()
	}
}

func (g *Gemini) send(message map[string]any) bool {
	g.mu.Lock()
	conn := g.conn
	g.mu.Unlock()
	if conn == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), writeTimeout)
	defer cancel()
	return conn.WriteJSON(ctx, message) == nil
}

func (g *Gemini) readLoop(ctx context.Context, conn *wsclient.Conn) {
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			g.handleClose(closeReason(err))
			return
		}
		g.handleMessage(parseObject(data))
	}
}

func (g *Gemini) handleMessage(message map[string]any) {
	h := g.cfg.Handlers
	if truthy(message, "setupComplete") {
		g.finishSetup(nil)
	}
	if update := obj(message, "sessionResumptionUpdate"); update != nil && truthy(update, "resumable") && str(update, "newHandle") != "" {
		g.mu.Lock()
		g.resumeHandle = str(update, "newHandle")
		g.mu.Unlock()
	}
	if content := obj(message, "serverContent"); content != nil {
		if text := str(obj(content, "inputTranscription"), "text"); text != "" {
			h.OnInputTranscript(text)
		}
		for _, part := range list(obj(content, "modelTurn"), "parts") {
			if data := str(obj(asMap(part), "inlineData"), "data"); data != "" {
				h.OnAudio(audiox.DecodeBase64(data))
			}
		}
		if text := str(obj(content, "outputTranscription"), "text"); text != "" {
			h.OnOutputTranscript(text)
		}
		if truthy(content, "interrupted") {
			h.OnInterrupted()
		}
		// Extended-thinking models end turns they are still working on.
		if truthy(content, "turnComplete") {
			h.OnTurnComplete(str(content, "interactionStatus") != "IN_PROGRESS")
		}
	}
	var responses []any
	for _, raw := range list(obj(message, "toolCall"), "functionCalls") {
		call := asMap(raw)
		name, id := str(call, "name"), str(call, "id")
		if name == g.cfg.HangUpTool {
			h.OnHangUp()
			continue
		}
		if id == "" {
			continue
		}
		args := obj(call, "args")
		if args == nil {
			args = map[string]any{}
		}
		if response, respond := h.OnToolCall(ToolCall{ID: id, Name: name, Args: args}); respond {
			responses = append(responses, map[string]any{"id": id, "name": name, "response": response})
		}
	}
	if len(responses) > 0 {
		g.send(map[string]any{"toolResponse": map[string]any{"functionResponses": responses}})
	}
	if truthy(message, "goAway") {
		g.handleClose("go_away")
		g.mu.Lock()
		conn := g.conn
		g.mu.Unlock()
		if conn != nil {
			go func() { _ = conn.Close(wsclient.CloseNormal, "") }()
		}
	}
	if truthy(message, "error") {
		text := str(obj(message, "error"), "message")
		if text == "" {
			text = "Gemini Live reported an error."
		}
		g.failOrReport(errors.New(text))
	}
}

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

// failOrReport rejects the pending setup, or reports the error to the session.
func (g *Gemini) failOrReport(err error) {
	if g.finishSetup(err) {
		return
	}
	g.cfg.Handlers.OnError(err)
}

// finishSetup settles a pending connect. It reports whether one was pending.
func (g *Gemini) finishSetup(err error) bool {
	g.mu.Lock()
	setup := g.setup
	g.setup = nil
	g.mu.Unlock()
	if setup == nil {
		return false
	}
	setup <- err
	return true
}

// handleClose: a connection that never completed setup is reported through
// Connect's error, not as a dropped session.
func (g *Gemini) handleClose(reason string) {
	neverStarted := g.finishSetup(fmt.Errorf("Gemini Live closed before setup completed (%s).", reason))
	g.mu.Lock()
	if g.closing || neverStarted {
		g.mu.Unlock()
		return
	}
	g.closing = true
	handle := g.resumeHandle
	g.mu.Unlock()
	g.cfg.Handlers.OnClosed(reason, handle)
}

func closeReason(err error) string {
	if reason := strings.TrimSpace(wsclient.CloseReason(err)); reason != "" {
		return reason
	}
	code := wsclient.CloseStatus(err)
	if code < 0 {
		code = 1006
	}
	return fmt.Sprintf("connection_closed_%d", code)
}

// sanitizeDialError keeps the API key, which Gemini takes in the URL, out of
// error texts that reach logs or the owner.
func sanitizeDialError(err error, secret string) error {
	if secret == "" || !strings.Contains(err.Error(), secret) {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), secret, "[key]"))
}
