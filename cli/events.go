// Package cli holds the parts every NeoLabs app CLI shares: the `--json` setup
// event stream, terminal output and prompts, port probes and a small command
// dispatcher. Service files, env files and signed releases live in the
// envfile, service and release subpackages.
package cli

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"slices"
	"strings"
	"time"
)

// Event is one line of the `--json` setup stream. Field order is the wire
// order desktop installers already parse.
type Event struct {
	SchemaVersion int         `json:"schemaVersion"`
	RunID         string      `json:"runId"`
	Profile       string      `json:"profile"`
	Stage         string      `json:"stage"`
	State         string      `json:"state"`
	Timestamp     string      `json:"timestamp,omitempty"`
	Progress      *float64    `json:"progress,omitempty"`
	Message       string      `json:"message,omitempty"`
	Error         *EventError `json:"error,omitempty"`
	Result        any         `json:"result,omitempty"`
}

// EventError says what failed and what the user can do about it.
type EventError struct {
	Code      string `json:"code"`
	Retryable bool   `json:"retryable"`
	Action    string `json:"action"`
	Detail    string `json:"detail"`
}

// EmitOptions are the optional parts of an event. Progress < 0 means none.
type EmitOptions struct {
	Stage    string
	Progress float64
	Message  string
	Error    *EventError
	Result   any
}

// Coded is an error that carries a machine-readable code.
type Coded interface {
	error
	Code() string
}

// CodedError is the plain Coded implementation.
type CodedError struct {
	code, msg string
}

// Errorf returns an error with a code.
func Errorf(code, format string, args ...any) error {
	return &CodedError{code: code, msg: fmt.Sprintf(format, args...)}
}

func (e *CodedError) Error() string { return e.msg }

// Code is the error's machine-readable code.
func (e *CodedError) Code() string { return e.code }

// CodeOf returns the code of err, or fallback.
func CodeOf(err error, fallback string) string {
	var c Coded
	if err != nil && asCoded(err, &c) && c.Code() != "" {
		return c.Code()
	}
	return fallback
}

func asCoded(err error, target *Coded) bool {
	for err != nil {
		if c, ok := err.(Coded); ok {
			*target = c
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// EventWriter writes setup events as JSON lines when JSON is on.
type EventWriter struct {
	SchemaVersion int
	Stages        []string
	Profile       string
	JSON          bool
	Out           io.Writer
	RunID         string
	Stage         string
	Now           func() time.Time
}

// NewEventWriter starts a run with a random run id at stage "prepare".
func NewEventWriter(schemaVersion int, stages []string, profile string, jsonOut bool, out io.Writer) *EventWriter {
	return &EventWriter{SchemaVersion: schemaVersion, Stages: stages, Profile: profile, JSON: jsonOut, Out: out, RunID: NewUUID(), Stage: "prepare", Now: time.Now}
}

// NewUUID returns a random version 4 UUID.
func NewUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// ISOTime formats t like JavaScript's Date.toISOString.
func ISOTime(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

func (w *EventWriter) checkStage(stage string) (string, error) {
	stage = strings.TrimSpace(stage)
	if !slices.Contains(w.Stages, stage) {
		if stage == "" {
			stage = "(empty)"
		}
		return "", Errorf("SETUP_EVENT_STAGE_INVALID", "Invalid setup event stage: %s.", stage)
	}
	return stage, nil
}

// Emit writes one event and returns it.
func (w *EventWriter) Emit(state string, o EmitOptions) (Event, error) {
	stage := o.Stage
	if stage == "" {
		stage = w.Stage
	}
	stage, err := w.checkStage(stage)
	if err != nil {
		return Event{}, err
	}
	now := time.Now
	if w.Now != nil {
		now = w.Now
	}
	ev := Event{SchemaVersion: w.SchemaVersion, RunID: w.RunID, Profile: w.Profile, Stage: stage, State: state, Timestamp: ISOTime(now()), Message: o.Message, Error: o.Error, Result: o.Result}
	if o.Progress >= 0 && !math.IsNaN(o.Progress) {
		p := math.Max(0, math.Min(1, o.Progress))
		ev.Progress = &p
	}
	if w.JSON && w.Out != nil {
		if err := WriteJSONLine(w.Out, ev); err != nil {
			return ev, err
		}
	}
	return ev, nil
}

// Start moves to stage and reports it started.
func (w *EventWriter) Start(stage, message string, progress float64) {
	if s, err := w.checkStage(stage); err == nil {
		w.Stage = s
		_, _ = w.Emit("started", EmitOptions{Stage: s, Message: message, Progress: progress})
	}
}

// Complete moves to stage and reports it completed.
func (w *EventWriter) Complete(stage, message string, progress float64) {
	if s, err := w.checkStage(stage); err == nil {
		w.Stage = s
		_, _ = w.Emit("completed", EmitOptions{Stage: s, Message: message, Progress: progress})
	}
}

// Message reports progress text at the current stage.
func (w *EventWriter) Message(message string) {
	_, _ = w.Emit("message", EmitOptions{Message: message, Progress: -1})
}

// Ready reports the final result of a command.
func (w *EventWriter) Ready(stage, message string, result any) {
	_, _ = w.Emit("ready", EmitOptions{Stage: stage, Progress: 1, Message: message, Result: result})
}

// Fail reports err at the current stage.
func (w *EventWriter) Fail(err error, retryable bool, action string) {
	detail := "Setup failed."
	if err != nil {
		detail = err.Error()
	}
	if action == "" {
		action = "retry"
	}
	_, _ = w.Emit("failed", EmitOptions{Progress: -1, Error: &EventError{Code: CodeOf(err, "SETUP_FAILED"), Retryable: retryable, Action: action, Detail: detail}})
}

// WriteJSONLine writes v as one compact JSON line without HTML escaping.
func WriteJSONLine(out io.Writer, v any) error {
	enc := json.NewEncoder(out)
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}
