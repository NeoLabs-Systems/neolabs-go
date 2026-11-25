package vm

// TotalMemoryBytes is the physical memory of this machine, or 0 when it
// cannot be read.
func TotalMemoryBytes() uint64 { return totalMemory() }
