// Package proc supervises subprocesses: it starts them in their own process
// group, ends a whole tree with SIGTERM then SIGKILL after a grace period,
// bounds the output it keeps, and reports how a process ended.
package proc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// DefaultGrace is how long a terminated process may take to exit on SIGTERM
// before its group is killed.
const DefaultGrace = 5 * time.Second

// Spec describes a process to start.
type Spec struct {
	Path string
	Args []string
	Dir  string
	// Env is the complete environment. Nil inherits the parent's.
	Env []string
	// Stdin is read by the process; nil means the null device, so a command that
	// reads input sees end of file instead of waiting.
	Stdin io.Reader
	// Stdout and Stderr receive the output; nil discards it.
	Stdout io.Writer
	Stderr io.Writer
}

// Process is a running subprocess.
type Process struct {
	cmd       *exec.Cmd
	pid       int
	done      chan struct{}
	waitErr   error
	reason    string
	mu        sync.Mutex
	grace     time.Duration
	forceKill *time.Timer
	// external ends a process that something else started, see Supervisor.Track.
	external func(reason string)
}

// Exit reports how a process ended.
type Exit struct {
	// Code is the exit status, or -1 when the process was ended by a signal.
	Code int
	// Signal is the terminating signal's name such as SIGTERM, empty for a normal exit.
	Signal string
	// Reason is the text given to Terminate, empty if nobody asked it to stop.
	Reason string
	// Err is a failure to wait for the process, never a non-zero exit.
	Err error
}

// Start launches the process in its own process group.
func Start(spec Spec) (*Process, error) {
	cmd := exec.Command(spec.Path, spec.Args...)
	cmd.Dir = spec.Dir
	cmd.Env = spec.Env
	cmd.Stdin = spec.Stdin
	cmd.Stdout = spec.Stdout
	cmd.Stderr = spec.Stderr
	cmd.SysProcAttr = groupAttr()
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	p := &Process{cmd: cmd, pid: cmd.Process.Pid, done: make(chan struct{}), grace: DefaultGrace}
	go func() {
		p.waitErr = cmd.Wait()
		close(p.done)
	}()
	return p, nil
}

// PID is the operating system process id.
func (p *Process) PID() int { return p.pid }

// Done is closed once the process has exited and its output is flushed.
func (p *Process) Done() <-chan struct{} { return p.done }

// Alive reports whether the process is still running.
func (p *Process) Alive() bool {
	select {
	case <-p.done:
		return false
	default:
		return true
	}
}

// Wait blocks until the process exits and reports how it ended.
func (p *Process) Wait() Exit {
	<-p.done
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.forceKill != nil {
		p.forceKill.Stop()
	}
	return p.exit()
}

func (p *Process) exit() Exit {
	out := Exit{Code: 0, Reason: p.reason}
	if p.waitErr == nil {
		return out
	}
	var ee *exec.ExitError
	if !errors.As(p.waitErr, &ee) {
		out.Code = -1
		out.Err = p.waitErr
		return out
	}
	out.Code = ee.ExitCode()
	if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		out.Signal = SignalName(ws.Signal())
	}
	return out
}

// Terminate asks the process group to stop with SIGTERM and kills it after the
// grace period if it is still running. The first reason given is the one
// reported by Wait.
func (p *Process) Terminate(reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.Alive() {
		return
	}
	if p.reason == "" {
		p.reason = reason
	}
	signalGroup(p.cmd.Process, syscall.SIGTERM)
	if p.forceKill == nil {
		p.forceKill = time.AfterFunc(p.grace, func() { p.Kill() })
	}
}

// Kill ends the process group at once.
func (p *Process) Kill() {
	if p.Alive() {
		signalGroup(p.cmd.Process, syscall.SIGKILL)
	}
}

// Signal sends sig to the process group.
func (p *Process) Signal(sig os.Signal) error {
	return signalGroup(p.cmd.Process, sig)
}

// Alive reports whether a process with this id exists, whoever started it.
func PIDAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return signalPID(pid, syscall.Signal(0)) == nil
}

// Result is the outcome of Run.
type Result struct {
	Exit
	Stdout   string
	Stderr   string
	TimedOut bool
}

// Run executes a short command to completion and keeps at most limit bytes of
// each output stream (the tail wins). It is for tools such as qemu-img whose
// output is read, not for long-lived processes.
func Run(ctx context.Context, timeout time.Duration, spec Spec, limit int) (Result, error) {
	if limit <= 0 {
		limit = 1 << 20
	}
	out, errBuf := &tailBuffer{limit: limit}, &tailBuffer{limit: limit}
	spec.Stdout, spec.Stderr = out, errBuf
	p, err := Start(spec)
	if err != nil {
		return Result{}, err
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		select {
		case <-ctx.Done():
			p.Terminate("timeout")
		case <-p.Done():
		}
	}()
	exit := p.Wait()
	<-stopped
	return Result{
		Exit:     exit,
		Stdout:   out.String(),
		Stderr:   errBuf.String(),
		TimedOut: ctx.Err() == context.DeadlineExceeded,
	}, nil
}

// Checked runs a command and returns its trimmed stdout, or an error that
// carries the command name and its error output.
func Checked(ctx context.Context, timeout time.Duration, spec Spec) (string, error) {
	res, err := Run(ctx, timeout, spec, 1<<20)
	if err != nil {
		return "", fmt.Errorf("%s: %w", spec.Path, err)
	}
	if res.Code != 0 || res.TimedOut {
		detail := string(bytes.TrimSpace([]byte(firstNonEmpty(res.Stderr, res.Stdout))))
		if res.TimedOut {
			detail = "timed out"
		}
		if detail == "" {
			detail = fmt.Sprintf("exit %d", res.Code)
		}
		return "", fmt.Errorf("%s: %s", spec.Path, detail)
	}
	return string(bytes.TrimSpace([]byte(res.Stdout))), nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// tailBuffer keeps the last limit bytes written to it.
type tailBuffer struct {
	mu    sync.Mutex
	limit int
	buf   []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.limit {
		t.buf = append([]byte(nil), t.buf[len(t.buf)-t.limit:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}

// Supervisor tracks started processes so they can be listed, killed by id and
// all stopped on shutdown.
type Supervisor struct {
	mu    sync.Mutex
	procs map[int]*Process
}

// NewSupervisor returns an empty supervisor.
func NewSupervisor() *Supervisor { return &Supervisor{procs: map[int]*Process{}} }

// Start launches a process and tracks it until it exits.
func (s *Supervisor) Start(spec Spec) (*Process, error) {
	p, err := Start(spec)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.procs[p.PID()] = p
	s.mu.Unlock()
	go func() {
		<-p.Done()
		s.mu.Lock()
		delete(s.procs, p.PID())
		s.mu.Unlock()
	}()
	return p, nil
}

// Track adds a process that something else started (a pty child, say) so Kill
// and KillAll reach it through stop. It is dropped again when done is closed.
func (s *Supervisor) Track(pid int, stop func(reason string), done <-chan struct{}) {
	s.mu.Lock()
	s.procs[pid] = &Process{pid: pid, done: make(chan struct{}), external: stop}
	s.mu.Unlock()
	go func() {
		<-done
		s.mu.Lock()
		delete(s.procs, pid)
		s.mu.Unlock()
	}()
}

// Managed reports whether pid is a tracked process.
func (s *Supervisor) Managed(pid int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.procs[pid]
	return ok
}

// Kill terminates a tracked process (SIGTERM, then SIGKILL after the grace
// period). It reports whether pid was tracked.
func (s *Supervisor) Kill(pid int, reason string) bool {
	s.mu.Lock()
	p := s.procs[pid]
	s.mu.Unlock()
	if p == nil {
		return false
	}
	p.stop(reason)
	return true
}

// KillAll terminates every tracked process.
func (s *Supervisor) KillAll(reason string) {
	s.mu.Lock()
	list := make([]*Process, 0, len(s.procs))
	for _, p := range s.procs {
		list = append(list, p)
	}
	s.mu.Unlock()
	for _, p := range list {
		p.stop(reason)
	}
}

func (p *Process) stop(reason string) {
	if p.external != nil {
		p.external(reason)
		return
	}
	p.Terminate(reason)
}

var signalNames = map[syscall.Signal]string{
	syscall.SIGHUP: "SIGHUP", syscall.SIGINT: "SIGINT", syscall.SIGQUIT: "SIGQUIT",
	syscall.SIGILL: "SIGILL", syscall.SIGTRAP: "SIGTRAP", syscall.SIGABRT: "SIGABRT",
	syscall.SIGBUS: "SIGBUS", syscall.SIGFPE: "SIGFPE", syscall.SIGKILL: "SIGKILL",
	syscall.SIGSEGV: "SIGSEGV", syscall.SIGPIPE: "SIGPIPE", syscall.SIGALRM: "SIGALRM",
	syscall.SIGTERM: "SIGTERM",
}

// SignalName spells a signal the way Node does (SIGTERM, SIGKILL), falling
// back to its number for the platform-specific ones.
func SignalName(sig syscall.Signal) string {
	if name, ok := signalNames[sig]; ok {
		return name
	}
	return fmt.Sprintf("SIG%d", int(sig))
}
