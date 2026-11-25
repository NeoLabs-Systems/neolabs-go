package release

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInvocationPathPreservesPathSymlink(t *testing.T) {
	bin := t.TempDir()
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	launcher := filepath.Join(bin, "neorecall")
	target := filepath.Join(t.TempDir(), "runtime", "bin", "neorecall")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("versioned"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, launcher); err != nil {
		t.Fatal(err)
	}
	if got := InvocationPath("neorecall"); got != launcher {
		t.Fatalf("invocation path = %q, want %q", got, launcher)
	}
}

func TestRefreshCommandLauncherOnlyReplacesInvokedLauncher(t *testing.T) {
	bin := t.TempDir()
	launcher := filepath.Join(bin, "neoagent")
	versioned := filepath.Join(t.TempDir(), "versioned")
	source := filepath.Join(t.TempDir(), "new")
	if err := os.WriteFile(versioned, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("new"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(versioned, launcher); err != nil {
		t.Fatal(err)
	}
	if replaced, err := RefreshCommandLauncher(versioned, launcher, source); err != nil || replaced {
		t.Fatalf("versioned invocation: replaced=%v err=%v", replaced, err)
	}
	if data, err := os.ReadFile(launcher); err != nil || string(data) != "old" {
		t.Fatalf("launcher after unrelated invocation = %q, %v", data, err)
	}
	if replaced, err := RefreshCommandLauncher(launcher, launcher, source); err != nil || !replaced {
		t.Fatalf("launcher invocation: replaced=%v err=%v", replaced, err)
	}
	if data, err := os.ReadFile(launcher); err != nil || string(data) != "new" {
		t.Fatalf("refreshed launcher = %q, %v", data, err)
	}
	if data, err := os.ReadFile(versioned); err != nil || string(data) != "old" {
		t.Fatalf("versioned runtime = %q, %v", data, err)
	}
}
