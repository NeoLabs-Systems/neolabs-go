package realtime

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// nodeModules finds the socket.io-client of the Node server. The tests that
// use it are skipped where node or the package are not available.
func nodeModules(t *testing.T) (node, modules string) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	for _, dir := range []string{os.Getenv("NEOAGENT_NODE_MODULES")} {
		if dir == "" {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, "socket.io-client", "package.json")); err == nil {
			return node, dir
		}
	}
	t.Skip("socket.io-client is not installed")
	return "", ""
}

func runJSClient(t *testing.T, mode string) map[string]any {
	t.Helper()
	node, modules := nodeModules(t)
	ts := newTestServer(t, Options{})
	script, _ := filepath.Abs("testdata/e2e_client.js")
	cmd := exec.Command(node, script, modules, ts.URL, mode)
	done := make(chan struct{})
	var out []byte
	var err error
	go func() { out, err = cmd.CombinedOutput(); close(done) }()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("js client timed out")
	}
	if err != nil {
		t.Fatalf("js client failed: %v\n%s", err, out)
	}
	var seen map[string]any
	if err := json.Unmarshal(out, &seen); err != nil {
		t.Fatalf("unexpected client output %q", out)
	}
	return seen
}

func TestJSClientWebsocketTransport(t *testing.T) {
	seen := runJSClient(t, "websocket")
	if seen["transport"] != "websocket" {
		t.Fatalf("%v", seen)
	}
}

func TestJSClientPollingTransport(t *testing.T) {
	seen := runJSClient(t, "polling")
	if seen["transport"] != "polling" {
		t.Fatalf("%v", seen)
	}
}

func TestJSClientUpgradesFromPolling(t *testing.T) {
	seen := runJSClient(t, "upgrade")
	if seen["transport"] != "polling" || seen["upgradedTo"] != "websocket" {
		t.Fatalf("%v", seen)
	}
}

func TestJSClientReconnectsAfterDrop(t *testing.T) {
	seen := runJSClient(t, "reconnect")
	if seen["reconnected"] != true || seen["dropReason"] != "transport close" {
		t.Fatalf("%v", seen)
	}
}

// TestDartClientUsedByTheFlutterApp runs the socket_io_client package of the
// Flutter app against the hub. It needs dart and the package in the pub cache.
func TestDartClientUsedByTheFlutterApp(t *testing.T) {
	dart, err := exec.LookPath("dart")
	if err != nil {
		t.Skip("dart is not installed")
	}
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	script, err := os.ReadFile("testdata/dart_client.dart")
	if err != nil {
		t.Fatal(err)
	}
	pubspec := "name: e2e\npublish_to: none\nenvironment:\n  sdk: ^3.0.0\ndependencies:\n  socket_io_client: ^3.1.2\n"
	for name, body := range map[string][]byte{"bin/main.dart": script, "pubspec.yaml": []byte(pubspec)} {
		if err := os.WriteFile(filepath.Join(dir, name), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	get := exec.Command(dart, "pub", "get", "--offline")
	get.Dir = dir
	if out, err := get.CombinedOutput(); err != nil {
		t.Skipf("socket_io_client is not in the pub cache: %s", out)
	}
	ts := newTestServer(t, Options{})
	run := exec.Command(dart, "run", "bin/main.dart", ts.URL)
	run.Dir = dir
	out, err := run.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "ok") {
		t.Fatalf("dart client failed: %v\n%s", err, out)
	}
}
