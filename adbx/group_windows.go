//go:build windows

package adbx

import (
	"os"
	"syscall"

	"golang.org/x/sys/windows"
)

// Windows has no process groups to signal; the command itself is ended.
func groupAttrs() *syscall.SysProcAttr { return &syscall.SysProcAttr{} }

func terminateGroup(p *os.Process) { _ = p.Kill() }

func killGroup(p *os.Process) { _ = p.Kill() }

func pidAlive(pid int) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	var code uint32
	return windows.GetExitCodeProcess(h, &code) == nil && code == 259 // STILL_ACTIVE
}
