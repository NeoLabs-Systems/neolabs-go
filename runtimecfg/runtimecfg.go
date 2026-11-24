// Package runtimecfg resolves where a NeoLabs app keeps its data and reads its
// environment. All apps share the same rules: a private (0700) home directory,
// private (0600) secret files, `.env` loading that never overrides the real
// environment.
package runtimecfg

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Paths are the directories and files of one app instance.
type Paths struct {
	Home         string
	DataDir      string
	AgentDataDir string
	LogDir       string
	EnvFile      string
}

// Resolve applies the app's overrides, e.g. prefix "NEOAGENT" reads
// NEOAGENT_HOME, NEOAGENT_DATA_DIR, NEOAGENT_AGENT_DATA_DIR and NEOAGENT_ENV_FILE,
// and defaults to ~/.neoagent (dotDir ".neoagent").
func Resolve(prefix, dotDir string, getenv func(string) string, userHome string) Paths {
	abs := func(p string) string {
		if a, err := filepath.Abs(p); err == nil {
			return a
		}
		return p
	}
	pick := func(key, fallback string) string {
		if v := strings.TrimSpace(getenv(prefix + "_" + key)); v != "" {
			return abs(v)
		}
		return abs(fallback)
	}
	home := pick("HOME", filepath.Join(userHome, dotDir))
	data := pick("DATA_DIR", filepath.Join(home, "data"))
	return Paths{
		Home:         home,
		DataDir:      data,
		AgentDataDir: pick("AGENT_DATA_DIR", filepath.Join(home, "agent-data")),
		LogDir:       filepath.Join(data, "logs"),
		EnvFile:      pick("ENV_FILE", filepath.Join(home, ".env")),
	}
}

// Ensure creates the directories with mode 0700.
func (p Paths) Ensure() error {
	for _, dir := range []string{p.Home, p.DataDir, p.LogDir, p.AgentDataDir} {
		if err := EnsurePrivateDir(dir); err != nil {
			return err
		}
	}
	return nil
}

// EnsurePrivateDir creates dir (0700) and tightens an existing one.
func EnsurePrivateDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("runtimecfg: create %s: %w", dir, err)
	}
	return os.Chmod(dir, 0o700)
}

// EnsurePrivateFile tightens an existing file to 0600; a missing file is fine.
func EnsurePrivateFile(path string) error {
	err := os.Chmod(path, 0o600)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// WritePrivateFile writes data atomically with mode 0600 (secrets, key files).
func WritePrivateFile(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// ParseEnv reads KEY=VALUE lines. Blank lines and `#` comments are skipped, an
// optional `export ` prefix is accepted and matching surrounding quotes are removed.
func ParseEnv(raw string) map[string]string {
	out := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(raw))
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		idx := strings.Index(line, "=")
		if idx <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:idx])
		value := strings.TrimSpace(line[idx+1:])
		if n := len(value); n >= 2 && (value[0] == '"' || value[0] == '\'') && value[n-1] == value[0] {
			value = value[1 : n-1]
		}
		out[key] = value
	}
	return out
}

// LoadEnvFile sets variables from the file that are not already set, like
// dotenv without override. A missing file is not an error.
func LoadEnvFile(path string, lookup func(string) (string, bool), set func(string, string) error) error {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for k, v := range ParseEnv(string(raw)) {
		if _, exists := lookup(k); !exists {
			if err := set(k, v); err != nil {
				return err
			}
		}
	}
	return nil
}

// Bool reads a boolean flag (1/true/yes/on, 0/false/no/off), else fallback.
func Bool(value string, fallback bool) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	return fallback
}
