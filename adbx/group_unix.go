//go:build !windows

package adbx

import (
	"errors"
	"os"
	"syscall"
)

// A command runs in its own process group, so a timeout also ends children
// the command spawned.
func groupAttrs() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setpgid: true} }

func terminateGroup(p *os.Process) { _ = syscall.Kill(-p.Pid, syscall.SIGTERM) }

func killGroup(p *os.Process) { _ = syscall.Kill(-p.Pid, syscall.SIGKILL) }

func pidAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
