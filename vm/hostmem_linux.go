//go:build linux

package vm

import (
	"bufio"
	"os"
	"strconv"
	"strings"
)

func totalMemory() uint64 {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) >= 2 && fields[0] == "MemTotal:" {
			kib, _ := strconv.ParseUint(fields[1], 10, 64)
			return kib * 1024
		}
	}
	return 0
}
