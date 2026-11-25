//go:build !windows

package service

import "syscall"

func detachedAttrs() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setsid: true} }

func processAlive(pid int) bool { return syscall.Kill(pid, 0) == nil }

func terminate(pid int) { _ = syscall.Kill(pid, syscall.SIGTERM) }
