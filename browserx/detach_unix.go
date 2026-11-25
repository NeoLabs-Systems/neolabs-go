//go:build !windows

package browserx

import "syscall"

// The browser gets its own session so it survives a terminal hangup.
func detachedAttrs() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setsid: true} }
