package livevoice

import "context"

// Message is one earlier turn of the conversation handed to a provider when a
// call starts.
type Message struct{ Role, Text string }

// ConnectParams is what a provider connection starts with.
type ConnectParams struct {
	Instructions string
	History      []Message
	// ResumeHandle continues a Gemini session across a dropped connection.
	ResumeHandle string
}

// ToolCall is a function call the model makes (Gemini Live).
type ToolCall struct {
	ID   string
	Name string
	Args map[string]any
}

// Handlers receive what a provider connection reports. The session wraps each
// one so events of a replaced connection are ignored.
type Handlers struct {
	OnAudio            func(pcm []byte)
	OnInputTranscript  func(text string)
	OnOutputTranscript func(text string)
	// OnInterrupted: the owner talked over the model.
	OnInterrupted func()
	// OnTurnComplete ends a model turn. Extended-thinking models end turns they
	// are still working on, which they report with idle false.
	OnTurnComplete func(idle bool)
	// OnToolCall answers a function call; respond false sends nothing back.
	OnToolCall func(call ToolCall) (response any, respond bool)
	// OnHangUp: the model called the hang-up tool.
	OnHangUp func()
	// OnDelegation reports a GPT-Live hand-off. Its request is empty: the model
	// does not name one.
	OnDelegation func(handle string) string
	OnError      func(err error)
	// OnClosed reports a connection that ended after it had started.
	OnClosed func(reason, resumeHandle string)
}

// Adapter is one live provider connection. Say and note take the handle of
// the hand-off they answer, which is empty for none.
type Adapter interface {
	Connect(ctx context.Context, p ConnectParams) error
	AppendAudio(pcm []byte)
	StartInput()
	EndInput()
	// Say makes the model tell the owner something now, in its own words.
	Say(text, handle string)
	// Note gives the model context it keeps quiet about.
	Note(text, handle string)
	Close()
	// ReportsTurnEnds says whether the provider marks the end of model turns.
	ReportsTurnEnds() bool
}
