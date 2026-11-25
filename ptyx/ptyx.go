// Package ptyx runs programs on pseudo-terminals in process: a Session owns
// one terminal, keeps a bounded scrollback, fans its output out to
// subscribers and can be resized, written to and ended. A Manager keeps the
// sessions of one application by id.
package ptyx

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/NeoLabs-Systems/neolabs-go/proc"
	"github.com/creack/pty"
)

// ErrClosed is returned for writes to a session whose process has exited.
var ErrClosed = errors.New("ptyx: session closed")

// DefaultScrollback is the number of output bytes a session remembers.
const DefaultScrollback = 256 * 1024

// DefaultGrace is how long Close waits after SIGTERM before it kills the
// process group.
const DefaultGrace = 3 * time.Second

// Config describes a session to start.
type Config struct {
	Path       string
	Args       []string
	Dir        string
	Env        []string
	Cols, Rows uint16
	// Scrollback is the replay buffer size in bytes; zero uses DefaultScrollback.
	Scrollback int
}

// Session is one program attached to a pseudo-terminal.
type Session struct {
	cmd  *exec.Cmd
	tty  *os.File
	done chan struct{}

	mu         sync.Mutex
	scrollback []byte
	limit      int
	subs       map[int]func([]byte)
	nextSub    int
	exit       Exit
	reason     string
}

// Exit reports how the session's process ended.
type Exit struct {
	Code   int
	Signal string
	// Reason is what Close was given, empty if nobody asked it to stop.
	Reason string
}

// Start launches the program on a new pseudo-terminal, in its own session so
// the whole process tree can be signalled.
func Start(cfg Config) (*Session, error) {
	if cfg.Cols == 0 {
		cfg.Cols = 120
	}
	if cfg.Rows == 0 {
		cfg.Rows = 30
	}
	limit := cfg.Scrollback
	if limit <= 0 {
		limit = DefaultScrollback
	}
	cmd := exec.Command(cfg.Path, cfg.Args...)
	cmd.Dir = cfg.Dir
	cmd.Env = cfg.Env
	tty, err := pty.StartWithAttrs(cmd, &pty.Winsize{Cols: cfg.Cols, Rows: cfg.Rows}, sessionAttr())
	if err != nil {
		return nil, err
	}
	s := &Session{cmd: cmd, tty: tty, done: make(chan struct{}), limit: limit, subs: map[int]func([]byte){}}
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		s.pump()
	}()
	go func() {
		err := cmd.Wait()
		// The terminal reports end of file once the last holder of the slave is
		// gone; let the reader drain what is left before announcing the exit.
		select {
		case <-readDone:
		case <-time.After(time.Second):
		}
		s.mu.Lock()
		s.exit = exitOf(err)
		s.exit.Reason = s.reason
		s.mu.Unlock()
		_ = tty.Close()
		close(s.done)
	}()
	return s, nil
}

func exitOf(err error) Exit {
	if err == nil {
		return Exit{}
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		return Exit{Code: -1}
	}
	out := Exit{Code: ee.ExitCode()}
	if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		out.Signal = proc.SignalName(ws.Signal())
	}
	return out
}

func (s *Session) pump() {
	buf := make([]byte, 32*1024)
	for {
		n, err := s.tty.Read(buf)
		if n > 0 {
			chunk := append([]byte(nil), buf[:n]...)
			s.mu.Lock()
			s.scrollback = append(s.scrollback, chunk...)
			if len(s.scrollback) > s.limit {
				s.scrollback = append([]byte(nil), s.scrollback[len(s.scrollback)-s.limit:]...)
			}
			subs := make([]func([]byte), 0, len(s.subs))
			for _, f := range s.subs {
				subs = append(subs, f)
			}
			s.mu.Unlock()
			for _, f := range subs {
				f(chunk)
			}
		}
		if err != nil {
			return
		}
	}
}

// PID is the process id of the program.
func (s *Session) PID() int { return s.cmd.Process.Pid }

// Done is closed after the program exited and its output was delivered.
func (s *Session) Done() <-chan struct{} { return s.done }

// Wait blocks until the session ends.
func (s *Session) Wait() Exit {
	<-s.done
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.exit
}

// Write sends input to the program.
func (s *Session) Write(p []byte) (int, error) {
	select {
	case <-s.done:
		return 0, ErrClosed
	default:
	}
	return s.tty.Write(p)
}

// Resize changes the terminal size.
func (s *Session) Resize(cols, rows uint16) error {
	return pty.Setsize(s.tty, &pty.Winsize{Cols: cols, Rows: rows})
}

// Scrollback returns a copy of the remembered output.
func (s *Session) Scrollback() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.scrollback...)
}

// Subscribe registers fn for output chunks after the call and returns a
// function that removes it. fn runs on the session's reader goroutine, so it
// must not block.
func (s *Session) Subscribe(fn func([]byte)) (unsubscribe func()) {
	s.mu.Lock()
	id := s.nextSub
	s.nextSub++
	s.subs[id] = fn
	s.mu.Unlock()
	return func() {
		s.mu.Lock()
		delete(s.subs, id)
		s.mu.Unlock()
	}
}

// Close ends the program: SIGTERM to its process group, SIGKILL after grace.
func (s *Session) Close(reason string, grace time.Duration) {
	select {
	case <-s.done:
		return
	default:
	}
	if grace <= 0 {
		grace = DefaultGrace
	}
	s.mu.Lock()
	if s.reason == "" {
		s.reason = reason
	}
	s.mu.Unlock()
	s.signal(syscall.SIGTERM)
	select {
	case <-s.done:
	case <-time.After(grace):
		s.signal(syscall.SIGKILL)
		<-s.done
	}
}

func (s *Session) signal(sig syscall.Signal) { signalGroup(s.cmd.Process, sig) }

// Manager keeps sessions by id.
type Manager struct {
	mu       sync.Mutex
	sessions map[string]*Session
	next     int
}

// NewManager returns an empty manager.
func NewManager() *Manager { return &Manager{sessions: map[string]*Session{}} }

// Start launches a session and returns it under a new id. The session leaves
// the manager when its program exits.
func (m *Manager) Start(cfg Config) (string, *Session, error) {
	s, err := Start(cfg)
	if err != nil {
		return "", nil, err
	}
	m.mu.Lock()
	m.next++
	id := fmt.Sprintf("pty-%d", m.next)
	m.sessions[id] = s
	m.mu.Unlock()
	go func() {
		<-s.Done()
		m.mu.Lock()
		delete(m.sessions, id)
		m.mu.Unlock()
	}()
	return id, s, nil
}

// Get returns a running session.
func (m *Manager) Get(id string) (*Session, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	return s, ok
}

// IDs lists the running sessions.
func (m *Manager) IDs() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.sessions))
	for id := range m.sessions {
		out = append(out, id)
	}
	return out
}

// CloseAll ends every session and waits for them.
func (m *Manager) CloseAll(reason string) {
	m.mu.Lock()
	list := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		list = append(list, s)
	}
	m.mu.Unlock()
	var wg sync.WaitGroup
	for _, s := range list {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.Close(reason, 0)
		}()
	}
	wg.Wait()
}

var _ io.Writer = (*Session)(nil)
