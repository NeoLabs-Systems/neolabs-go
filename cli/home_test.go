package cli

import "testing"

func TestHomeActionsFollowRunningState(t *testing.T) {
	noop := func() error { return nil }
	h := HomeActions{AppName: "Test", Open: noop, Start: noop, Status: noop, Restart: noop, Update: noop, Help: noop}
	actions := h.actions()
	if len(actions) != 3 || actions[0].choice.Label != "Start Test" {
		t.Fatalf("stopped actions: %+v", actions)
	}
	h.Running, h.UpdateVersion = true, "2.0.0"
	actions = h.actions()
	if len(actions) != 5 || actions[0].choice.Label != "Open Test in my browser" || actions[1].choice.Hint != "2.0.0 is ready" || actions[3].choice.Label != "Restart Test" {
		t.Fatalf("running actions: %+v", actions)
	}
}
