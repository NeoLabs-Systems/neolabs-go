//go:build windows

package proc

import (
	"os"
	"syscall"
)

func groupAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}

// signalGroup kills the process: Windows has no signal groups to deliver to.
func signalGroup(p *os.Process, _ os.Signal) error { return p.Kill() }

func signalPID(pid int, _ syscall.Signal) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Release()
}
