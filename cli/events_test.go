package cli

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestEventWireFormat(t *testing.T) {
	var out bytes.Buffer
	w := NewEventWriter(1, []string{"prepare", "complete"}, "quick", true, &out)
	w.RunID = "run"
	w.Now = func() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 6e6, time.UTC) }
	w.Start("prepare", "Checking", 1.7)
	w.Ready("complete", "done", map[string]any{"backendUrl": "http://localhost:3333"})
	w.Fail(fmt.Errorf("wrapped: %w", Errorf("SETUP_PORT_IN_USE", "port <busy>")), true, "")
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	want := []string{
		`{"schemaVersion":1,"runId":"run","profile":"quick","stage":"prepare","state":"started","timestamp":"2026-01-02T03:04:05.006Z","progress":1,"message":"Checking"}`,
		`{"schemaVersion":1,"runId":"run","profile":"quick","stage":"complete","state":"ready","timestamp":"2026-01-02T03:04:05.006Z","progress":1,"message":"done","result":{"backendUrl":"http://localhost:3333"}}`,
		`{"schemaVersion":1,"runId":"run","profile":"quick","stage":"prepare","state":"failed","timestamp":"2026-01-02T03:04:05.006Z","error":{"code":"SETUP_PORT_IN_USE","retryable":true,"action":"retry","detail":"wrapped: port <busy>"}}`,
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Errorf("line %d\n got %s\nwant %s", i, lines[i], want[i])
		}
	}
	if _, err := w.Emit("started", EmitOptions{Stage: "bogus"}); CodeOf(err, "") != "SETUP_EVENT_STAGE_INVALID" {
		t.Fatal(err)
	}
	if CodeOf(errors.New("x"), "FALLBACK") != "FALLBACK" {
		t.Fatal("fallback code")
	}
}

func TestFlagsAndPositionals(t *testing.T) {
	args := []string{"--channel", "beta", "list", "--json", "--x=1", "KEY"}
	if v, ok := FlagValue(args, "--channel"); !ok || v != "beta" {
		t.Fatal(v)
	}
	if v, _ := FlagValue(args, "--x"); v != "1" {
		t.Fatal(v)
	}
	if p := Positionals(args, "--channel"); strings.Join(p, ",") != "list,KEY" {
		t.Fatal(p)
	}
}

func TestFindAvailablePort(t *testing.T) {
	p, err := FindAvailablePort(43123, 3333)
	if err != nil || p < 43123 {
		t.Fatal(p, err)
	}
}
