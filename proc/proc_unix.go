//go:build !windows

package proc

import (
	"os"
	"syscall"
)

func groupAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setpgid: true} }

// signalGroup signals the process group, falling back to the process itself.
func signalGroup(p *os.Process, sig os.Signal) error {
	if s, ok := sig.(syscall.Signal); ok {
		if err := syscall.Kill(-p.Pid, s); err == nil {
			return nil
		}
	}
	return p.Signal(sig)
}

func signalPID(pid int, sig syscall.Signal) error { return syscall.Kill(pid, sig) }
