// Package envfile reads and edits an app's `.env` file the way the NeoLabs
// CLIs always have: KEY=VALUE lines, comments and unknown lines kept in place,
// every write atomic and private (0600).
package envfile

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

var keyPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Parse reads KEY=VALUE lines. Keys are trimmed, values are kept verbatim;
// blank lines, `#` comments and lines without `=` are skipped. A later line
// wins over an earlier one with the same key.
func Parse(raw string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		idx := strings.Index(line, "=")
		if idx < 0 {
			continue
		}
		if key := strings.TrimSpace(line[:idx]); key != "" {
			out[key] = line[idx+1:]
		}
	}
	return out
}

// SortedKeys returns the keys of values in order.
func SortedKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Read returns the file's text, or "" when it does not exist.
func Read(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	return string(raw), err
}

// Load parses the file; a missing file is empty.
func Load(path string) (map[string]string, error) {
	raw, err := Read(path)
	if err != nil {
		return nil, err
	}
	return Parse(raw), nil
}

// ValidKey reports whether key may be written to an env file.
func ValidKey(key string) bool { return keyPattern.MatchString(key) }

func checkKey(key string) (string, error) {
	key = strings.NewReplacer("\r", "", "\n", "").Replace(key)
	if !ValidKey(key) {
		if key == "" {
			key = "(empty)"
		}
		return "", fmt.Errorf("Invalid environment variable name: %s", key)
	}
	return key, nil
}

// joinLines drops one trailing empty line and ends the text with a newline.
func joinLines(lines []string) string {
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	return strings.Join(lines, "\n") + "\n"
}

// Upsert replaces the first KEY= line or appends one. Newlines in the value
// are removed so a value can never inject another key.
func Upsert(path, key, value string) error {
	key, err := checkKey(key)
	if err != nil {
		return err
	}
	value = strings.NewReplacer("\r", "", "\n", "").Replace(value)
	raw, err := Read(path)
	if err != nil {
		return err
	}
	var lines []string
	if raw != "" {
		lines = strings.Split(raw, "\n")
	}
	replaced := false
	for i, line := range lines {
		if strings.HasPrefix(line, key+"=") {
			lines[i] = key + "=" + value
			replaced = true
			break
		}
	}
	if !replaced {
		if n := len(lines); n > 0 && lines[n-1] == "" {
			lines = lines[:n-1]
		}
		lines = append(lines, key+"="+value)
	}
	return WriteAtomic(path, []byte(joinLines(lines)), 0o600)
}

// Remove deletes every KEY= line; false when the file does not exist.
func Remove(path, key string) (bool, error) {
	key, err := checkKey(key)
	if err != nil {
		return false, err
	}
	raw, err := Read(path)
	if err != nil || raw == "" {
		return false, err
	}
	var kept []string
	for _, line := range strings.Split(raw, "\n") {
		if !strings.HasPrefix(line, key+"=") {
			kept = append(kept, line)
		}
	}
	return true, WriteAtomic(path, []byte(joinLines(kept)), 0o600)
}

// Merge applies updates to raw: the first line of each updated key is
// rewritten in place, later duplicates are dropped, and new keys are appended
// in the order given.
func Merge(raw string, keys []string, updates map[string]string) string {
	pending := map[string]string{}
	for k, v := range updates {
		pending[k] = v
	}
	seen := map[string]bool{}
	var merged []string
	linePattern := regexp.MustCompile(`^([A-Z][A-Z0-9_]*)=`)
	for _, line := range strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n") {
		m := linePattern.FindStringSubmatch(line)
		if m == nil {
			merged = append(merged, line)
			continue
		}
		if _, updated := updates[m[1]]; !updated {
			merged = append(merged, line)
			continue
		}
		if seen[m[1]] {
			continue
		}
		merged = append(merged, m[1]+"="+updates[m[1]])
		seen[m[1]] = true
		delete(pending, m[1])
	}
	for len(merged) > 0 && merged[len(merged)-1] == "" {
		merged = merged[:len(merged)-1]
	}
	for _, k := range keys {
		if v, ok := pending[k]; ok {
			merged = append(merged, k+"="+v)
			delete(pending, k)
		}
	}
	for _, k := range SortedKeys(pending) {
		merged = append(merged, k+"="+pending[k])
	}
	return strings.Join(merged, "\n") + "\n"
}

// Mask hides a secret-looking value (its key names a key, token, secret or
// password) except for its first and last four characters.
func Mask(key, value string) string {
	upper := strings.ToUpper(key)
	if !strings.Contains(upper, "KEY") && !strings.Contains(upper, "TOKEN") &&
		!strings.Contains(upper, "SECRET") && !strings.Contains(upper, "PASSWORD") {
		return value
	}
	if len(value) <= 8 {
		return "********"
	}
	return value[:4] + "..." + value[len(value)-4:]
}

// WriteAtomic writes data to a fresh sibling file (created exclusively with
// mode), syncs it and renames it over path. On systems where rename cannot
// replace an existing file the old file is moved aside first and restored if
// the swap fails.
func WriteAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp := sibling(path, "tmp")
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	defer os.Remove(tmp)
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err == nil {
		return nil
	} else if _, statErr := os.Stat(path); statErr != nil {
		return err
	}
	backup := sibling(path, "backup")
	if err := os.Rename(path, backup); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		if _, statErr := os.Stat(path); statErr != nil {
			_ = os.Rename(backup, path)
		}
		return err
	}
	_ = os.Remove(backup)
	return nil
}

func sibling(path, suffix string) string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return filepath.Join(filepath.Dir(path), fmt.Sprintf(".%s.%d.%s.%s", filepath.Base(path), os.Getpid(), hex.EncodeToString(b[:]), suffix))
}
