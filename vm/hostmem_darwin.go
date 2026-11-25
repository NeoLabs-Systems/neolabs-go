//go:build darwin

package vm

import "golang.org/x/sys/unix"

func totalMemory() uint64 {
	bytes, err := unix.SysctlUint64("hw.memsize")
	if err != nil {
		return 0
	}
	return bytes
}
