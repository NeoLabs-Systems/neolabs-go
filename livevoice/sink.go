package livevoice

// Sink carries a call's events to its client. Every live voice client shares
// one vocabulary; a transport only supplies how an event reaches its client:
// socket.io for the app, a raw WebSocket for the wearable.
//
// Server to client events: voice:session_ready, voice:state, voice:audio,
// voice:interrupted, voice:transcript, voice:task, voice:error.
type Sink interface {
	// Send delivers kind ("audio", "state", ...) with its payload.
	Send(kind string, payload map[string]any)
}

// SinkFunc adapts a delivery function to a Sink, naming events "voice:<kind>".
type SinkFunc func(event string, payload map[string]any)

// Send implements Sink.
func (f SinkFunc) Send(kind string, payload map[string]any) { f("voice:"+kind, payload) }
