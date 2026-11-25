package proc

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestRunCapturesOutputAndExitCode(t *testing.T) {
	res, err := Run(context.Background(), 5*time.Second, Spec{Path: "/bin/sh", Args: []string{"-c", "echo out; echo err >&2; exit 3"}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.Code != 3 || strings.TrimSpace(res.Stdout) != "out" || strings.TrimSpace(res.Stderr) != "err" {
		t.Fatalf("unexpected result %+v", res)
	}
}

func TestRunKeepsOnlyTheTail(t *testing.T) {
	res, err := Run(context.Background(), 5*time.Second, Spec{Path: "/bin/sh", Args: []string{"-c", "printf 0123456789"}}, 4)
	if err != nil {
		t.Fatal(err)
	}
	if res.Stdout != "6789" {
		t.Fatalf("tail = %q", res.Stdout)
	}
}

func TestRunTimeoutKillsTheWholeGroup(t *testing.T) {
	start := time.Now()
	res, err := Run(context.Background(), 200*time.Millisecond, Spec{Path: "/bin/sh", Args: []string{"-c", "sleep 30 & sleep 30"}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !res.TimedOut || res.Reason != "timeout" || res.Signal == "" {
		t.Fatalf("expected a timeout kill, got %+v", res)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatal("timeout did not end the process promptly")
	}
}

func TestStdinNilIsEndOfFile(t *testing.T) {
	res, err := Run(context.Background(), 5*time.Second, Spec{Path: "/bin/sh", Args: []string{"-c", "cat; echo done"}}, 0)
	if err != nil || strings.TrimSpace(res.Stdout) != "done" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

func TestTerminateEscalatesToKill(t *testing.T) {
	out := &tailBuffer{limit: 1024}
	p, err := Start(Spec{Path: "/bin/sh", Args: []string{"-c", "trap '' TERM; echo ready; while :; do sleep 1; done"}, Stdout: out})
	if err != nil {
		t.Fatal(err)
	}
	p.grace = 200 * time.Millisecond
	for !strings.Contains(out.String(), "ready") {
		time.Sleep(10 * time.Millisecond)
	}
	p.Terminate("aborted")
	select {
	case <-p.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("process ignored SIGTERM and was never killed")
	}
	if e := p.Wait(); e.Signal != "SIGKILL" || e.Reason != "aborted" {
		t.Fatalf("exit = %+v", e)
	}
}

func TestSupervisorKillsTrackedProcesses(t *testing.T) {
	s := NewSupervisor()
	p, err := s.Start(Spec{Path: "/bin/sh", Args: []string{"-c", "sleep 30"}})
	if err != nil {
		t.Fatal(err)
	}
	if !s.Managed(p.PID()) {
		t.Fatal("started process is not tracked")
	}
	if s.Kill(999999, "x") {
		t.Fatal("unknown pid reported as killed")
	}
	s.KillAll("shutdown")
	if e := p.Wait(); e.Reason != "shutdown" {
		t.Fatalf("exit = %+v", e)
	}
	time.Sleep(50 * time.Millisecond)
	if s.Managed(p.PID()) {
		t.Fatal("finished process is still tracked")
	}
}

func TestCheckedReportsStderr(t *testing.T) {
	_, err := Checked(context.Background(), 5*time.Second, Spec{Path: "/bin/sh", Args: []string{"-c", "echo boom >&2; exit 1"}})
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v", err)
	}
	got, err := Checked(context.Background(), 5*time.Second, Spec{Path: "/bin/sh", Args: []string{"-c", "echo hi"}})
	if err != nil || got != "hi" {
		t.Fatalf("got %q err %v", got, err)
	}
}

func TestPIDAlive(t *testing.T) {
	p, _ := Start(Spec{Path: "/bin/sh", Args: []string{"-c", "sleep 30"}})
	if !PIDAlive(p.PID()) {
		t.Fatal("running pid not alive")
	}
	p.Kill()
	p.Wait()
	if PIDAlive(p.PID()) || PIDAlive(0) {
		t.Fatal("dead pid reported alive")
	}
}
