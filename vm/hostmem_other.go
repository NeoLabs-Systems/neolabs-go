//go:build !linux && !darwin && !windows

package vm

func totalMemory() uint64 { return 0 }
