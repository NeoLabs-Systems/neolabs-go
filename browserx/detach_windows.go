//go:build windows

package browserx

import (
	"syscall"

	"golang.org/x/sys/windows"
)

func detachedAttrs() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP}
}
