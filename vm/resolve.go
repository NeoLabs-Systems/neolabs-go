package vm

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// ExecutableName adds the platform's executable suffix.
func ExecutableName(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}

// SystemBinaryName is the qemu system emulator for a guest architecture
// ("arm64" or anything else for x86-64).
func SystemBinaryName(architecture string) string {
	if architecture == "arm64" {
		return "qemu-system-aarch64"
	}
	return "qemu-system-x86_64"
}

// IsRunnableFile reports whether candidate resolves to an executable file.
func IsRunnableFile(candidate string) bool {
	resolved, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return false
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	if runtime.GOOS == "windows" {
		return true
	}
	return info.Mode().Perm()&0o111 != 0
}

// CommandExists reports whether name is on PATH.
func CommandExists(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

// IsRunnableCommand accepts either a path to an executable or a name on PATH.
func IsRunnableCommand(command string) bool {
	value := strings.TrimSpace(command)
	if value == "" {
		return false
	}
	if strings.ContainsRune(value, filepath.Separator) || (len(value) > 2 && value[1] == ':' && (value[2] == '\\' || value[2] == '/')) {
		return IsRunnableFile(value)
	}
	return CommandExists(value)
}

// ResolveExecutable returns the first runnable candidate, else name itself
// when it is on PATH, else "".
func ResolveExecutable(name string, candidates []string) string {
	for _, candidate := range candidates {
		if candidate = strings.TrimSpace(candidate); candidate != "" && IsRunnableFile(candidate) {
			return candidate
		}
	}
	if CommandExists(name) {
		return name
	}
	return ""
}

// FirstExisting returns the first path that exists, or "".
func FirstExisting(candidates []string) string {
	for _, candidate := range candidates {
		if candidate != "" {
			if _, err := os.Stat(candidate); err == nil {
				return candidate
			}
		}
	}
	return ""
}
