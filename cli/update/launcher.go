package update

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/NeoLabs-Systems/neolabs-go/cli/release"
)

// RefreshLauncher updates the standalone command install.sh put in binDir
// (default ~/.local/bin) to the installed runtime's binary. Other installation
// methods are left alone.
func RefreshLauncher(invoked, binDir, name, source string) error {
	if binDir = strings.TrimSpace(binDir); binDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		binDir = filepath.Join(home, ".local", "bin")
	}
	launcher, err := filepath.Abs(filepath.Join(binDir, name))
	if err != nil {
		return err
	}
	_, err = release.RefreshCommandLauncher(invoked, launcher, source)
	return err
}
