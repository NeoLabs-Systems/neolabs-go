package livevoice

import (
	"sync"
	"time"
)

// Timings of the floor, as the Node server had them.
const (
	// QuietLine is how long nobody must have spoken before a progress update
	// may fill the silence.
	QuietLine = 6 * time.Second
	// ReplyTimeout is how long replies the model owes but does not give hold
	// the line (it may stay silent, or answer two things in one turn).
	ReplyTimeout = 6 * time.Second
	// WorkingTimeout bounds the thinking and tool calls between a turn marked
	// in progress and the idle one.
	WorkingTimeout = 30 * time.Second
)

// Floor tracks who has the line in a live call. What the server puts on it (a
// task's outcome, a progress update, context) must not cut into the owner's
// sentence or the model's answer: Gemini drops the answer it is generating
// when a new turn arrives. Deliveries wait for a free line, and a progress
// update is only spoken into a line that has been quiet for a while.
//
// The methods are called with the owner's lock held. Only the timers take the
// lock themselves (Lock is what they acquire), so a Floor and the session that
// drives it share one mutex.
type Floor struct {
	// OnReplyMissed is called when the model owed a reply and the owner heard
	// nothing at all.
	OnReplyMissed func()

	clock Clock
	lock  sync.Locker

	held      []func()
	connected bool
	speaking  bool
	inputHeld bool
	// ownerWaiting: the owner said something the model has not answered yet.
	ownerWaiting bool
	// repliesDue counts tool results and told messages the model answers, a
	// turn each.
	repliesDue    int
	spokeThisTurn bool
	// heard: the model has made a sound since the owner last spoke or it was
	// last told something. It may answer before its tool call as well as after.
	heard        bool
	working      bool
	lastSpeechAt time.Time
	replyTimer   Timer
	workingTimer Timer
}

// NewFloor builds a floor whose timers lock lock before they touch it.
func NewFloor(clock Clock, lock sync.Locker) *Floor {
	return &Floor{clock: clock, lock: lock}
}

// Free reports whether something can be said to the owner now.
func (f *Floor) Free() bool {
	return f.connected && !f.speaking && !f.inputHeld && !f.ownerWaiting && f.repliesDue == 0 && !f.working
}

// Quiet: nobody has said anything for a while, so an update fills a silence
// instead of interrupting a conversation.
func (f *Floor) Quiet() bool {
	return f.Free() && f.SilentFor() >= QuietLine
}

// SilentFor is how long it has been since anyone spoke.
func (f *Floor) SilentFor() time.Duration {
	return f.clock.Now().Sub(f.lastSpeechAt)
}

// OwnerWaiting reports an owner turn the model has not answered yet.
func (f *Floor) OwnerWaiting() bool { return f.ownerWaiting }

// WhenFree runs deliver as soon as the line is free.
func (f *Floor) WhenFree(deliver func()) {
	f.held = append(f.held, deliver)
	f.release()
}

// SetConnected tells the floor about the provider connection. A dropped one
// ends whatever the model was saying or doing; the next starts with nothing
// pending.
func (f *Floor) SetConnected(connected bool) {
	f.connected = connected
	if !connected {
		// A button released while the line was down is never reported.
		f.inputHeld = false
		f.speaking = false
		f.spokeThisTurn = false
		f.settleReplies()
		f.settleWorking()
	}
	f.release()
}

// ModelSpeaking: the model's audio arrives faster than it plays; the owner
// hears it until playbackEndsAt.
func (f *Floor) ModelSpeaking(playbackEndsAt time.Time) {
	f.speaking = true
	f.heard = true
	f.spokeThisTurn = true
	f.lastSpeechAt = laterOf(f.clock.Now(), playbackEndsAt)
}

// DroppedSpeech: speech the owner stopped is dropped, but it holds the line
// until it ends and answers nothing.
func (f *Floor) DroppedSpeech() {
	f.speaking = true
	f.heard = true
}

// ModelSilent: the model stopped producing audio.
func (f *Floor) ModelSilent() {
	f.speaking = false
	if f.ownerWaiting || f.repliesDue > 0 {
		f.armReplyTimeout()
	}
	f.release()
}

// SetInputHeld marks the owner's push-to-talk turn; hands-free speech shows up
// as their transcript instead.
func (f *Floor) SetInputHeld(held bool) {
	f.inputHeld = held
	f.lastSpeechAt = f.clock.Now()
	if !held {
		f.OwnerSpoke()
	}
	f.release()
}

// OwnerSpoke: the owner said something the model owes an answer to.
func (f *Floor) OwnerSpoke() {
	f.lastSpeechAt = f.clock.Now()
	f.heard = false
	f.ownerWaiting = true
	f.armReplyTimeout()
}

// OwnerInterrupted: the owner talked over the model, so what it said so far
// answers nothing.
func (f *Floor) OwnerInterrupted() {
	f.speaking = false
	f.spokeThisTurn = false
	f.OwnerSpoke()
}

// ExpectReply: the model was handed something it answers in a turn of its own,
// a tool result or a delivery it is told to speak.
func (f *Floor) ExpectReply(told bool) {
	if told {
		f.heard = false
	}
	f.repliesDue++
	f.armReplyTimeout()
}

// TurnEnded: a turn the model spoke in answers the owner and one thing it
// owed. Gemini ends the turn of a tool call before it speaks, which settles
// nothing.
func (f *Floor) TurnEnded() {
	if f.spokeThisTurn {
		f.spokeThisTurn = false
		f.ownerWaiting = false
		if f.repliesDue > 0 {
			f.repliesDue--
		}
	}
	f.release()
}

// DropUnstartedReply gives up on a reply the model has not started: a new turn
// replaces it. It reports whether it did.
func (f *Floor) DropUnstartedReply() bool {
	if f.speaking || f.heard || f.ownerWaiting {
		return false
	}
	f.settleReplies()
	return true
}

// ModelWorking: Gemini's extended-thinking models end turns while they go on
// thinking or calling tools; the line stays theirs until they report idle.
func (f *Floor) ModelWorking() {
	f.working = true
	stop(f.workingTimer)
	f.workingTimer = f.clock.AfterFunc(WorkingTimeout, func() {
		f.lock.Lock()
		defer f.lock.Unlock()
		f.ModelIdle()
	})
}

// ModelIdle ends the model's working state.
func (f *Floor) ModelIdle() {
	f.settleWorking()
	f.release()
}

// Close drops everything held.
func (f *Floor) Close() {
	f.held = nil
	f.connected = false
	f.settleReplies()
	f.settleWorking()
}

func (f *Floor) armReplyTimeout() {
	stop(f.replyTimer)
	f.replyTimer = f.clock.AfterFunc(ReplyTimeout, func() {
		f.lock.Lock()
		defer f.lock.Unlock()
		missed := f.repliesDue > 0 && !f.heard && !f.working
		f.settleReplies()
		if missed && f.OnReplyMissed != nil {
			f.OnReplyMissed()
		}
		f.release()
	})
}

func (f *Floor) settleReplies() {
	f.ownerWaiting = false
	f.repliesDue = 0
	stop(f.replyTimer)
}

func (f *Floor) settleWorking() {
	f.working = false
	stop(f.workingTimer)
}

// release delivers held items while the line is free. A delivery that makes
// the model speak calls ExpectReply, so the next one waits for that answer.
func (f *Floor) release() {
	for len(f.held) > 0 && f.Free() {
		next := f.held[0]
		f.held = f.held[1:]
		next()
	}
}

func laterOf(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}
