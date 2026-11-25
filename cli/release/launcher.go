package release

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// InvocationPath resolves argv0 to an absolute path without resolving
// symlinks. That preserves the command path a user invoked when it points at
// an immutable versioned runtime binary.
func InvocationPath(argv0 string) string {
	if argv0 == "" {
		return ""
	}
	path := argv0
	if !filepath.IsAbs(path) && !strings.ContainsRune(path, filepath.Separator) {
		if found, err := exec.LookPath(path); err == nil {
			path = found
		}
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return ""
	}
	return filepath.Clean(absolute)
}

// RefreshCommandLauncher replaces launcherPath with sourcePath only when the
// current process was invoked through that launcher. It returns false without
// error when the app was started through another installation path.
func RefreshCommandLauncher(invokedPath, launcherPath, sourcePath string) (bool, error) {
	invoked, err := filepath.Abs(invokedPath)
	if err != nil {
		return false, err
	}
	launcher, err := filepath.Abs(launcherPath)
	if err != nil {
		return false, err
	}
	if filepath.Clean(invoked) != filepath.Clean(launcher) {
		return false, nil
	}
	source, err := os.Open(sourcePath)
	if err != nil {
		return false, err
	}
	defer source.Close()
	temp, err := os.CreateTemp(filepath.Dir(launcher), ".launcher-update-*")
	if err != nil {
		return false, err
	}
	defer os.Remove(temp.Name())
	_, copyErr := io.Copy(temp, source)
	if copyErr == nil {
		copyErr = temp.Chmod(0o755)
	}
	if closeErr := temp.Close(); copyErr == nil {
		copyErr = closeErr
	}
	if copyErr != nil {
		return false, copyErr
	}
	if err := os.Rename(temp.Name(), launcher); err != nil {
		return false, err
	}
	return true, nil
}
