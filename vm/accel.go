package vm

import (
	"context"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/NeoLabs-Systems/neolabs-go/proc"
)

var accelName = regexp.MustCompile(`^[a-z][a-z0-9]*$`)

// ParseAccelerators reads the names out of `qemu -accel help`.
func ParseAccelerators(output string) []string {
	var names []string
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 && accelName.MatchString(fields[0]) {
			names = append(names, fields[0])
		}
	}
	return names
}

// PreferredAccelerator is the hardware accelerator for a host OS (runtime.GOOS).
func PreferredAccelerator(goos string) string {
	switch goos {
	case "darwin":
		return "hvf"
	case "windows":
		return "whpx"
	default:
		return "kvm"
	}
}

// SelectAccelerators returns the accelerator to use: the host's hardware one
// when qemu offers it and it is usable, else tcg, else none. usable vets a
// hardware accelerator (kvm needs /dev/kvm, hvf needs the signed binary).
func SelectAccelerators(ctx context.Context, qemuBinary, goos string, usable func(accelerator string) bool) []string {
	var available []string
	if out, err := proc.Checked(ctx, 10*time.Second, proc.Spec{Path: qemuBinary, Args: []string{"-accel", "help"}}); err == nil {
		available = ParseAccelerators(out)
	}
	preferred := PreferredAccelerator(goos)
	if contains(available, preferred) && usable(preferred) {
		return []string{preferred}
	}
	if contains(available, "tcg") {
		return []string{"tcg"}
	}
	return nil
}

func contains(list []string, value string) bool {
	for _, v := range list {
		if v == value {
			return true
		}
	}
	return false
}

// KVMUsable reports whether /dev/kvm can be opened for reading and writing.
func KVMUsable() bool {
	f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
	if err != nil {
		return false
	}
	f.Close()
	return true
}
