package vm

import (
	"context"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/NeoLabs-Systems/neolabs-go/proc"
)

// FindOrphanedPIDs returns every qemu process whose command line mentions
// instanceDir. A qemu that outlived the server holds the write lock on its
// disks, so the next start fails on every image operation. Only a process whose
// command line still points at this instance is ours to reclaim. Each restart
// that loses track of its qemu leaves another one holding the same disks and
// its own frozen screen, so all of them are returned, not just the last.
func FindOrphanedPIDs(ctx context.Context, instanceDir string) []int {
	res, err := proc.Run(ctx, 10*time.Second, proc.Spec{Path: "ps", Args: []string{"-axo", "pid=,command="}}, 8<<20)
	if err != nil || res.Code != 0 {
		return nil
	}
	return parseOrphans(res.Stdout, instanceDir, os.Getpid())
}

func parseOrphans(psOutput, instanceDir string, self int) []int {
	var pids []int
	for _, line := range strings.Split(psOutput, "\n") {
		if !strings.Contains(line, "qemu-system-") || !strings.Contains(line, instanceDir) {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if pid, err := strconv.Atoi(fields[0]); err == nil && pid > 1 && pid != self {
			pids = append(pids, pid)
		}
	}
	return pids
}

// TerminatePID stops a process we did not start: SIGTERM, then SIGKILL after
// five seconds. It reports whether the process is gone.
func TerminatePID(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return true
	}
	_ = p.Signal(syscall.SIGTERM)
	if waitGone(pid, 5*time.Second) {
		return true
	}
	_ = p.Kill()
	return waitGone(pid, 3*time.Second)
}

func waitGone(pid int, wait time.Duration) bool {
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		if !proc.PIDAlive(pid) {
			return true
		}
		time.Sleep(200 * time.Millisecond)
	}
	return !proc.PIDAlive(pid)
}
