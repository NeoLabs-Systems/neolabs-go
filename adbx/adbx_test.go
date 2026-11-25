package adbx

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeAdb writes a shell script that records its arguments and behaves like adb
// for the few commands the tests use.
func fakeAdb(t *testing.T) (bin, log string) {
	t.Helper()
	dir := t.TempDir()
	log = filepath.Join(dir, "args.log")
	bin = filepath.Join(dir, "adb")
	script := `#!/bin/sh
echo "$@" >> "` + log + `"
case "$3" in
  shell) printf 'out:%s' "$4" ;;
  exec-out) printf '\211PNG\r\n' ;;
  fail) echo "device offline" >&2; exit 1 ;;
  sleep) sleep 5 ;;
  flood) head -c 5000 /dev/zero ;;
esac
`
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return bin, log
}

func TestShellAndExecOut(t *testing.T) {
	bin, log := fakeAdb(t)
	c := Client{Bin: bin, Serial: "emulator-5554"}
	out, err := c.Shell(context.Background(), "input tap 1 2", RunOptions{})
	if err != nil || out != "out:input tap 1 2" {
		t.Fatalf("shell = %q, %v", out, err)
	}
	raw, err := c.ExecOut(context.Background(), RunOptions{}, "screencap", "-p")
	if err != nil || string(raw) != "\x89PNG\r\n" {
		t.Fatalf("exec-out = %q, %v", raw, err)
	}
	logged, _ := os.ReadFile(log)
	if !strings.Contains(string(logged), "-s emulator-5554 shell input tap 1 2") {
		t.Fatalf("args log: %s", logged)
	}
}

func TestProcessFailures(t *testing.T) {
	bin, _ := fakeAdb(t)
	c := Client{Bin: bin, Serial: "x"}

	_, err := c.Run(context.Background(), RunOptions{}, "fail")
	var pe *ProcessError
	if !errors.As(err, &pe) || pe.Code != CodeExit || pe.Message != "device offline" || pe.Result.ExitCode != 1 {
		t.Fatalf("exit error = %#v", err)
	}

	_, err = c.Run(context.Background(), RunOptions{Timeout: 150 * time.Millisecond}, "sleep")
	if !errors.As(err, &pe) || pe.Code != CodeTimeout || pe.Message != "Command timed out after 150 ms." {
		t.Fatalf("timeout error = %#v", err)
	}

	_, err = c.Run(context.Background(), RunOptions{MaxOutputBytes: 1024}, "flood")
	if !errors.As(err, &pe) || pe.Code != CodeOutputLimit || pe.Message != "Command exceeded the 1024-byte output limit." {
		t.Fatalf("limit error = %#v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	if _, err = c.Run(ctx, RunOptions{}, "sleep"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error = %v", err)
	}
}

func TestStdinAndPid(t *testing.T) {
	res, err := Run(context.Background(), "cat", nil, RunOptions{Stdin: []byte("hello")})
	if err != nil || string(res.Stdout) != "hello" {
		t.Fatalf("cat = %q, %v", res.Stdout, err)
	}
	if !PidAlive(os.Getpid()) || PidAlive(0) || PidAlive(-3) {
		t.Fatal("PidAlive")
	}
}

func TestBinPath(t *testing.T) {
	if got := BinPath("/sdk"); got != filepath.Join("/sdk", "platform-tools", "adb") {
		t.Fatal(got)
	}
	if BinPath("") != "adb" {
		t.Fatal("bare name")
	}
}
