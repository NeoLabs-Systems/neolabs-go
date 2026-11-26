package update

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/NeoLabs-Systems/neolabs-go/cli/release"
)

func launcherPath(binDir, name string) (string, error) {
	if binDir = strings.TrimSpace(binDir); binDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		binDir = filepath.Join(home, ".local", "bin")
	}
	return filepath.Abs(filepath.Join(binDir, name))
}

// RefreshLauncher updates the standalone command install.sh put in binDir
// (default ~/.local/bin) to the installed runtime's binary. Other installation
// methods are left alone.
func RefreshLauncher(invoked, binDir, name, source string) error {
	launcher, err := launcherPath(binDir, name)
	if err != nil {
		return err
	}
	_, err = release.RefreshCommandLauncher(invoked, launcher, source)
	return err
}

// HealLauncher replaces a command that an earlier updater left behind: some
// releases switched the runtime without refreshing it, so every later `update`
// ran the old program again. The new binary calls this about itself. Only a
// regular file that reports an older version than version is replaced.
func HealLauncher(ctx context.Context, binDir, name, source, version string) (bool, error) {
	launcher, err := launcherPath(binDir, name)
	if err != nil {
		return false, err
	}
	info, err := os.Lstat(launcher)
	if os.IsNotExist(err) || err == nil && !info.Mode().IsRegular() {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, launcher, "version").Output()
	if err != nil {
		// Something that cannot say what it is stays as it is.
		return false, nil
	}
	found, err := release.NormalizeVersion(strings.TrimSpace(string(out)))
	if err != nil || release.CompareVersions(found, version) >= 0 {
		return false, nil
	}
	return release.RefreshCommandLauncher(launcher, launcher, source)
}
