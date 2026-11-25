//go:build !windows

package ptyx

import (
	"os"
	"syscall"
)

// sessionAttr makes the program a session leader with the terminal as its
// controlling terminal, so signals reach its whole process group.
func sessionAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setsid: true, Setctty: true} }

func signalGroup(p *os.Process, sig syscall.Signal) {
	if err := syscall.Kill(-p.Pid, sig); err != nil {
		_ = p.Signal(sig)
	}
}
