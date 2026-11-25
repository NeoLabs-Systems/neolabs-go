//go:build windows

package ptyx

import (
	"os"
	"syscall"
)

func sessionAttr() *syscall.SysProcAttr { return nil }

func signalGroup(p *os.Process, _ syscall.Signal) { _ = p.Kill() }
