// Package adbx runs subprocesses with a deadline and an output cap, and wraps
// the adb command line for one Android device.
package adbx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Limits applied when RunOptions leaves a field at zero.
const (
	DefaultTimeout        = 20 * time.Second
	DefaultMaxOutputBytes = 4 << 20
	minTimeout            = 100 * time.Millisecond
	maxTimeout            = 30 * time.Minute
	minOutputBytes        = 1024
	maxOutputBytes        = 1 << 30
	killGrace             = time.Second
)

// Error codes carried by *ProcessError.
const (
	CodeExit        = "PROCESS_EXIT"
	CodeTimeout     = "PROCESS_TIMEOUT"
	CodeOutputLimit = "PROCESS_OUTPUT_LIMIT"
)

// RunOptions tune Run. Zero values select the defaults; out-of-range values
// are clamped.
type RunOptions struct {
	Timeout        time.Duration
	MaxOutputBytes int
	Dir            string
	// Env replaces the process environment when non-nil.
	Env   []string
	Stdin []byte
}

// Result is what a finished process wrote.
type Result struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
	// Signal names the signal that ended the process, if any.
	Signal string
}

// ProcessError reports a process that did not finish successfully. For a
// non-zero exit Result holds the captured output.
type ProcessError struct {
	Code    string
	Message string
	Result  Result
}

func (e *ProcessError) Error() string { return e.Message }

// Clamp bounds v to [min, max], returning fallback when v is zero.
func Clamp(v, fallback, min, max int) int {
	if v == 0 {
		return fallback
	}
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}

// limitedBuffer counts what both pipes write and stops accepting once the
// shared cap is exceeded.
type limitedBuffer struct {
	mu      *sync.Mutex
	total   *int
	max     int
	over    func()
	buf     bytes.Buffer
	tripped bool
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.tripped {
		return len(p), nil
	}
	*l.total += len(p)
	if *l.total > l.max {
		l.tripped = true
		l.over()
		return len(p), nil
	}
	l.buf.Write(p)
	return len(p), nil
}

// Run executes name with args and waits for it. Cancelling ctx terminates the
// process (SIGTERM, then SIGKILL after a second) and returns ctx's error.
func Run(ctx context.Context, name string, args []string, o RunOptions) (Result, error) {
	timeout := time.Duration(Clamp(int(o.Timeout), int(DefaultTimeout), int(minTimeout), int(maxTimeout)))
	limit := Clamp(o.MaxOutputBytes, DefaultMaxOutputBytes, minOutputBytes, maxOutputBytes)
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	runCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	cmd := exec.Command(name, args...)
	cmd.Dir = o.Dir
	cmd.Env = o.Env
	cmd.Stdin = bytes.NewReader(o.Stdin)
	// Own process group, so a timeout also ends children the command spawned.
	cmd.SysProcAttr = groupAttrs()
	cmd.WaitDelay = killGrace

	var mu sync.Mutex
	total := 0
	overLimit := func() {
		cancel(&ProcessError{
			Code:    CodeOutputLimit,
			Message: fmt.Sprintf("Command exceeded the %d-byte output limit.", limit),
		})
	}
	stdout := &limitedBuffer{mu: &mu, total: &total, max: limit, over: overLimit}
	stderr := &limitedBuffer{mu: &mu, total: &total, max: limit, over: overLimit}
	cmd.Stdout, cmd.Stderr = stdout, stderr

	if err := cmd.Start(); err != nil {
		return Result{}, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	timer := time.AfterFunc(timeout, func() {
		cancel(&ProcessError{
			Code:    CodeTimeout,
			Message: fmt.Sprintf("Command timed out after %d ms.", timeout.Milliseconds()),
		})
	})
	defer timer.Stop()

	var waitErr error
	select {
	case waitErr = <-done:
	case <-runCtx.Done():
		terminateGroup(cmd.Process)
		select {
		case <-done:
		case <-time.After(killGrace):
			killGroup(cmd.Process)
			<-done
		}
		cause := context.Cause(runCtx)
		var pe *ProcessError
		if errors.As(cause, &pe) {
			return Result{}, pe
		}
		return Result{}, ctx.Err()
	}

	mu.Lock()
	res := Result{Stdout: stdout.buf.Bytes(), Stderr: stderr.buf.Bytes()}
	mu.Unlock()
	var exitErr *exec.ExitError
	switch {
	case waitErr == nil:
		return res, nil
	case errors.As(waitErr, &exitErr):
		res.ExitCode = exitErr.ExitCode()
		if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			res.Signal = ws.Signal().String()
		}
		msg := strings.TrimSpace(string(res.Stderr))
		if msg == "" {
			msg = strings.TrimSpace(string(res.Stdout))
		}
		if msg == "" {
			msg = fmt.Sprintf("Command exited with code %d.", res.ExitCode)
		}
		return res, &ProcessError{Code: CodeExit, Message: msg, Result: res}
	default:
		return res, waitErr
	}
}
