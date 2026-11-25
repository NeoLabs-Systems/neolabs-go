//go:build !windows

package vm

import (
	"os"
	"syscall"
)

func ownedPrivately(info os.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Getuid() && info.Mode().Perm()&0o077 == 0
}
