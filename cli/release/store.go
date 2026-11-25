package release

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/NeoLabs-Systems/neolabs-go/cli/envfile"
)

// RuntimeStore manages versioned runtime packages for an application.
// RequiredFiles are relative paths within a runtime's version directory.
type RuntimeStore struct {
	Root          string
	RequiredFiles []string
	SchemaVersion int
}

type RuntimeMarker struct {
	SchemaVersion int    `json:"schemaVersion"`
	Version       string `json:"version"`
	ActivatedAt   string `json:"activatedAt"`
}

func (s RuntimeStore) versionsDir() string { return filepath.Join(s.Root, "versions") }
func (s RuntimeStore) markerPath() string  { return filepath.Join(s.Root, "current.json") }

// RuntimeDir validates the version before using it as a directory name.
func (s RuntimeStore) RuntimeDir(version string) (string, error) {
	v, err := NormalizeVersion(version)
	if err != nil || v != version {
		return "", fmt.Errorf("invalid runtime version %q", version)
	}
	return filepath.Join(s.versionsDir(), v), nil
}

// Validate checks all required files without allowing paths outside the runtime.
func (s RuntimeStore) Validate(version string) error {
	dir, err := s.RuntimeDir(version)
	if err != nil {
		return err
	}
	return s.validateDir(dir)
}

func (s RuntimeStore) validateDir(dir string) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	for _, name := range s.RequiredFiles {
		if filepath.IsAbs(name) || name == "" {
			return fmt.Errorf("invalid required runtime file %q", name)
		}
		f, err := root.Open(name)
		if err != nil {
			return fmt.Errorf("runtime file %s: %w", name, err)
		}
		st, statErr := f.Stat()
		closeErr := f.Close()
		if statErr != nil {
			return statErr
		}
		if closeErr != nil {
			return closeErr
		}
		if !st.Mode().IsRegular() {
			return fmt.Errorf("runtime file %s is not a regular file", name)
		}
	}
	return nil
}

// InstallArchive stages and validates an already verified archive before
// installing it. An active version is never replaced, even if incomplete.
func (s RuntimeStore) InstallArchive(version, archive string) error {
	target, err := s.RuntimeDir(version)
	if err != nil {
		return err
	}
	if raw, err := os.ReadFile(s.markerPath()); err == nil {
		var marker RuntimeMarker
		if json.Unmarshal(raw, &marker) != nil {
			return fmt.Errorf("cannot install with an invalid active runtime marker")
		}
		if marker.Version == version {
			return fmt.Errorf("cannot replace the active runtime %s", version)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(s.versionsDir(), 0o700); err != nil {
		return err
	}
	staging, err := os.MkdirTemp(s.versionsDir(), ".staging-")
	if err != nil {
		return err
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.RemoveAll(staging)
		}
	}()
	extracted := filepath.Join(staging, "extracted")
	if err := ExtractZip(archive, extracted); err != nil {
		return err
	}
	if err := s.validateDir(extracted); err != nil {
		return err
	}
	backup := filepath.Join(staging, "previous")
	hadTarget := false
	if err := os.Rename(target, backup); err == nil {
		hadTarget = true
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.Rename(extracted, target); err != nil {
		if hadTarget {
			if restoreErr := os.Rename(backup, target); restoreErr != nil {
				cleanup = false
				return fmt.Errorf("install runtime: %v; previous directory remains at %s: %w", err, backup, restoreErr)
			}
		}
		return err
	}
	return nil
}

func (s RuntimeStore) Current() (RuntimeMarker, error) {
	var marker RuntimeMarker
	data, err := os.ReadFile(s.markerPath())
	if err != nil {
		return marker, err
	}
	if err := json.Unmarshal(data, &marker); err != nil {
		return marker, err
	}
	if err := s.Validate(marker.Version); err != nil {
		return marker, err
	}
	return marker, nil
}

// Activate updates the marker only after checking that the runtime is complete.
func (s RuntimeStore) Activate(version string) error {
	if err := s.Validate(version); err != nil {
		return err
	}
	data, err := json.Marshal(RuntimeMarker{SchemaVersion: s.SchemaVersion, Version: version, ActivatedAt: time.Now().UTC().Format(time.RFC3339Nano)})
	if err != nil {
		return err
	}
	return envfile.WriteAtomic(s.markerPath(), append(data, '\n'), 0o600)
}

// Prune removes version directories other than the active and rollback versions.
// Non-version entries and symlinks are left alone. Call only after verification.
func (s RuntimeStore) Prune(active, previous string) error {
	current, err := s.Current()
	if err != nil {
		return err
	}
	if current.Version != active {
		return fmt.Errorf("refusing cleanup: active runtime changed")
	}
	entries, err := os.ReadDir(s.versionsDir())
	if err != nil {
		return err
	}
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() || name == active || name == previous || name[0] < '0' || name[0] > '9' {
			continue
		}
		if v, err := NormalizeVersion(name); err != nil || v != name || strings.HasPrefix(name, ".") {
			continue
		}
		if err := os.RemoveAll(filepath.Join(s.versionsDir(), name)); err != nil {
			return err
		}
	}
	return nil
}
