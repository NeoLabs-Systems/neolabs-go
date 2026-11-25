package release

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRuntimeStoreActivationAndCleanup(t *testing.T) {
	s := RuntimeStore{Root: t.TempDir(), RequiredFiles: []string{"app/bin/app", "app/public/index.html"}, SchemaVersion: 1}
	for _, v := range []string{"1.0.0", "2.0.0", "3.0.0"} {
		d, err := s.RuntimeDir(v)
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range s.RequiredFiles {
			p := filepath.Join(d, name)
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte(v), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := s.Activate("3.0.0"); err != nil {
		t.Fatal(err)
	}
	if err := s.Activate("4.0.0"); err == nil {
		t.Fatal("activated missing runtime")
	}
	if err := s.Prune("2.0.0", "1.0.0"); err == nil {
		t.Fatal("pruned with stale active version")
	}
	if err := s.Prune("3.0.0", "2.0.0"); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		version string
		exists  bool
	}{{"1.0.0", false}, {"2.0.0", true}, {"3.0.0", true}} {
		d, _ := s.RuntimeDir(tc.version)
		_, err := os.Stat(d)
		if (err == nil) != tc.exists {
			t.Fatalf("runtime %s: %v", tc.version, err)
		}
	}
	if _, err := s.RuntimeDir("../escape"); err == nil {
		t.Fatal("accepted traversal")
	}
}

func TestRuntimeStoreInstallArchive(t *testing.T) {
	s := RuntimeStore{Root: t.TempDir(), RequiredFiles: []string{"app/bin/app", "app/public/index.html"}, SchemaVersion: 1}
	stage := t.TempDir()
	for _, name := range s.RequiredFiles {
		p := filepath.Join(stage, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("release"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	archive := filepath.Join(t.TempDir(), "runtime.zip")
	if err := CreateZip(stage, archive); err != nil {
		t.Fatal(err)
	}
	if err := s.InstallArchive("1.0.0", archive); err != nil {
		t.Fatal(err)
	}
	if err := s.Activate("1.0.0"); err != nil {
		t.Fatal(err)
	}
	if err := s.InstallArchive("1.0.0", archive); err == nil {
		t.Fatal("replaced active runtime")
	}
	incomplete := t.TempDir()
	if err := os.WriteFile(filepath.Join(incomplete, "other"), []byte("bad"), 0o600); err != nil {
		t.Fatal(err)
	}
	badArchive := filepath.Join(t.TempDir(), "bad.zip")
	if err := CreateZip(incomplete, badArchive); err != nil {
		t.Fatal(err)
	}
	if err := s.InstallArchive("2.0.0", badArchive); err == nil {
		t.Fatal("accepted incomplete runtime")
	}
	if marker, err := s.Current(); err != nil || marker.Version != "1.0.0" {
		t.Fatalf("current: %+v, %v", marker, err)
	}
}
