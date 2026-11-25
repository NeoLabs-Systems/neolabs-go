package livevoice

import (
	"encoding/json"
	"os"
	"sync"
	"testing"
	"time"
)

type floorStep struct {
	Op  string `json:"op"`
	Arg int    `json:"arg"`
}

type floorState struct {
	Returned     *bool  `json:"returned"`
	Free         bool   `json:"free"`
	Quiet        bool   `json:"quiet"`
	OwnerWaiting bool   `json:"ownerWaiting"`
	Delivered    int    `json:"delivered"`
	Missed       int    `json:"missed"`
	SilentForMs  *int64 `json:"silentForMs"`
}

// floor.json holds seeded random scripts and what the Node LiveFloor reported
// after each step (gen/floor.js, mocked timers). The Go floor must report the
// same in virtual time.
func TestFloorMatchesNode(t *testing.T) {
	raw, err := os.ReadFile("testdata/floor.json")
	if err != nil {
		t.Fatal(err)
	}
	var scenarios []struct {
		Steps []floorStep  `json:"steps"`
		Trace []floorState `json:"trace"`
	}
	if err := json.Unmarshal(raw, &scenarios); err != nil {
		t.Fatal(err)
	}
	for n, sc := range scenarios {
		clock := newFakeClock(time.UnixMilli(1000000))
		var mu sync.Mutex
		floor := NewFloor(clock, &mu)
		missed, delivered := 0, 0
		floor.OnReplyMissed = func() { missed++ }
		for i, step := range sc.Steps {
			var returned *bool
			mu.Lock()
			switch step.Op {
			case "connect":
				floor.SetConnected(true)
			case "disconnect":
				floor.SetConnected(false)
			case "speaking":
				floor.ModelSpeaking(clock.Now().Add(time.Duration(step.Arg) * time.Millisecond))
			case "dropped":
				floor.DroppedSpeech()
			case "silent":
				floor.ModelSilent()
			case "inputHeld":
				floor.SetInputHeld(true)
			case "inputReleased":
				floor.SetInputHeld(false)
			case "ownerSpoke":
				floor.OwnerSpoke()
			case "ownerInterrupted":
				floor.OwnerInterrupted()
			case "expect":
				floor.ExpectReply(false)
			case "expectTold":
				floor.ExpectReply(true)
			case "turnEnded":
				floor.TurnEnded()
			case "dropUnstarted":
				r := floor.DropUnstartedReply()
				returned = &r
			case "working":
				floor.ModelWorking()
			case "idle":
				floor.ModelIdle()
			case "whenFree":
				floor.WhenFree(func() { delivered++ })
			case "tick":
				mu.Unlock()
				clock.Advance(time.Duration(step.Arg) * time.Millisecond)
				mu.Lock()
			default:
				t.Fatalf("unknown op %q", step.Op)
			}
			want := sc.Trace[i]
			if (returned == nil) != (want.Returned == nil) || (returned != nil && *returned != *want.Returned) {
				t.Fatalf("scenario %d step %d (%s): returned %v, Node %v", n, i, step.Op, returned, want.Returned)
			}
			if floor.Free() != want.Free || floor.Quiet() != want.Quiet || floor.OwnerWaiting() != want.OwnerWaiting ||
				delivered != want.Delivered || missed != want.Missed {
				t.Fatalf("scenario %d step %d (%s %d): free=%v quiet=%v waiting=%v delivered=%d missed=%d; Node free=%v quiet=%v waiting=%v delivered=%d missed=%d",
					n, i, step.Op, step.Arg, floor.Free(), floor.Quiet(), floor.OwnerWaiting(), delivered, missed,
					want.Free, want.Quiet, want.OwnerWaiting, want.Delivered, want.Missed)
			}
			if want.SilentForMs != nil && floor.SilentFor().Milliseconds() != *want.SilentForMs {
				t.Fatalf("scenario %d step %d: silent for %d ms, Node %d", n, i, floor.SilentFor().Milliseconds(), *want.SilentForMs)
			}
			mu.Unlock()
		}
	}
}
