package update

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestHealLauncherReplacesOnlyAnOlderCommand(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell scripts")
	}
	script := func(version string) []byte { return []byte("#!/bin/sh\necho " + version + "\n") }
	for _, c := range []struct {
		have   []byte
		healed bool
	}{
		{script("3.9.1-beta.1"), true},
		{script("3.9.1-beta.14"), false},
		{script("3.10.0"), false},
		{[]byte("#!/bin/sh\nexit 1\n"), false},
		{nil, false},
	} {
		dir := t.TempDir()
		launcher, source := filepath.Join(dir, "app"), filepath.Join(t.TempDir(), "app")
		if err := os.WriteFile(source, script("3.9.1-beta.14"), 0o755); err != nil {
			t.Fatal(err)
		}
		if c.have != nil {
			if err := os.WriteFile(launcher, c.have, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		healed, err := HealLauncher(context.Background(), dir, "app", source, "3.9.1-beta.14")
		if err != nil || healed != c.healed {
			t.Errorf("%q: healed %v, err %v", c.have, healed, err)
		}
		if _, err := os.Stat(launcher); c.have == nil && !os.IsNotExist(err) {
			t.Error("a command that was not installed was created")
		}
	}
}
