package ptyx

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"
)

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestSessionRunsAShellOnATerminal(t *testing.T) {
	s, err := Start(Config{Path: "/bin/sh", Args: []string{"-c", "tty >/dev/null && echo is-a-tty; stty size"}})
	if err != nil {
		t.Fatal(err)
	}
	exit := s.Wait()
	out := string(s.Scrollback())
	if exit.Code != 0 || !strings.Contains(out, "is-a-tty") || !strings.Contains(out, "30 120") {
		t.Fatalf("exit=%+v out=%q", exit, out)
	}
}

func TestInteractiveInputAndSubscribers(t *testing.T) {
	s, err := Start(Config{Path: "/bin/sh", Args: []string{"-c", "read name; echo hello-$name"}})
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var live bytes.Buffer
	unsub := s.Subscribe(func(b []byte) { mu.Lock(); live.Write(b); mu.Unlock() })
	defer unsub()
	if _, err := s.Write([]byte("neo\r")); err != nil {
		t.Fatal(err)
	}
	s.Wait()
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(live.String(), "hello-neo") || !strings.Contains(string(s.Scrollback()), "hello-neo") {
		t.Fatalf("live=%q scrollback=%q", live.String(), s.Scrollback())
	}
	if _, err := s.Write([]byte("x")); err != ErrClosed {
		t.Fatalf("write after exit = %v", err)
	}
}

func TestResize(t *testing.T) {
	s, err := Start(Config{Path: "/bin/sh", Args: []string{"-c", "read x; stty size"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Resize(80, 24); err != nil {
		t.Fatal(err)
	}
	s.Write([]byte("\r"))
	s.Wait()
	if !strings.Contains(string(s.Scrollback()), "24 80") {
		t.Fatalf("scrollback %q", s.Scrollback())
	}
}

func TestScrollbackIsBounded(t *testing.T) {
	s, err := Start(Config{Path: "/bin/sh", Args: []string{"-c", "head -c 5000 /dev/zero | tr '\\0' a"}, Scrollback: 100})
	if err != nil {
		t.Fatal(err)
	}
	s.Wait()
	if n := len(s.Scrollback()); n != 100 {
		t.Fatalf("scrollback = %d bytes", n)
	}
}

func TestCloseEndsTheProcessGroupAndManagerForgetsIt(t *testing.T) {
	m := NewManager()
	id, s, err := m.Start(Config{Path: "/bin/sh", Args: []string{"-c", "sleep 60 & sleep 60"}})
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := m.Get(id); !ok || got != s {
		t.Fatal("started session is not registered")
	}
	m.CloseAll("shutdown")
	if e := s.Wait(); e.Reason != "shutdown" || e.Signal == "" {
		t.Fatalf("exit = %+v", e)
	}
	waitFor(t, "manager to drop the session", func() bool { _, ok := m.Get(id); return !ok })
}

func TestStartFailsForAMissingProgram(t *testing.T) {
	if _, err := Start(Config{Path: "/nonexistent/program"}); err == nil {
		t.Fatal("expected an error")
	}
}
