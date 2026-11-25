//go:build !windows

package vm

import "syscall"

func diskCapacity(directory string) (int64, int64) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(directory, &st); err != nil {
		return 0, 0
	}
	bsize := int64(st.Bsize)
	return int64(st.Bavail) * bsize, int64(st.Blocks) * bsize
}
