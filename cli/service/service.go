// Package service installs a NeoLabs app as a per-user background service:
// a launchd agent on macOS, a systemd user unit on Linux, a logon task on
// Windows, and a detached process everywhere else. Nothing here needs root.
package service

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/NeoLabs-Systems/neolabs-go/cli/envfile"
)

// EnvVar is one environment variable of the service, kept in order.
type EnvVar struct{ Key, Value string }

// Spec describes the service.
type Spec struct {
	// Label is the launchd label (e.g. "com.example"), Name the systemd unit
	// name, TaskName the Windows scheduled task.
	Label, Name, TaskName string
	Description           string
	Program               string
	Args                  []string
	WorkDir               string
	Env                   []EnvVar
	// EnvFile is loaded by systemd when present (the app reads it itself too).
	EnvFile string
	// LogDir receives <LogName>.log and <LogName>.error.log.
	LogDir, LogName string
	PIDFile         string
	// StopTimeout is how long the service manager waits for the program to
	// finish after asking it to stop; 0 keeps the manager's default.
	StopTimeout time.Duration
}

// Runner runs system commands; tests replace it.
type Runner interface {
	// Run runs a command and returns its combined output.
	Run(name string, args ...string) (string, error)
	Exists(name string) bool
}

// ExecRunner runs real commands.
type ExecRunner struct{}

// Run runs name with args.
func (ExecRunner) Run(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		tail := strings.TrimSpace(string(out))
		if tail != "" {
			return string(out), fmt.Errorf("%s %s: %w\n%s", name, strings.Join(args, " "), err, lastLines(tail, 12))
		}
		return string(out), fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return string(out), nil
}

// Exists reports whether name is on PATH.
func (ExecRunner) Exists(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

func lastLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// Manager installs and controls the service on one platform.
type Manager struct {
	Spec     Spec
	Platform string // "macos", "linux", "windows" or anything else
	Runner   Runner
	// PlistPath and UnitPath default to the per-user locations.
	PlistPath, UnitPath string
	// UID is the user id for launchctl's gui/<uid> domain; -1 if unknown.
	UID  int
	Logf func(level, msg string)
}

// New returns a Manager for the current user and platform.
func New(spec Spec, platform string) (*Manager, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	return &Manager{
		Spec:      spec,
		Platform:  platform,
		Runner:    ExecRunner{},
		PlistPath: filepath.Join(home, "Library", "LaunchAgents", spec.Label+".plist"),
		UnitPath:  filepath.Join(home, ".config", "systemd", "user", spec.Name+".service"),
		UID:       os.Getuid(),
	}, nil
}

func (m *Manager) log(level, msg string) {
	if m.Logf != nil {
		m.Logf(level, msg)
	}
}

func (m *Manager) logFile(suffix string) string {
	return filepath.Join(m.Spec.LogDir, m.Spec.LogName+suffix)
}

func (m *Manager) domain() string {
	if m.UID < 0 {
		return ""
	}
	return "gui/" + strconv.Itoa(m.UID)
}

func (m *Manager) target() string {
	if d := m.domain(); d != "" {
		return d + "/" + m.Spec.Label
	}
	return m.Spec.Label
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// Installed reports whether a launchd/systemd definition exists.
func (m *Manager) Installed() bool {
	switch m.Platform {
	case "macos":
		return exists(m.PlistPath)
	case "linux":
		return exists(m.UnitPath)
	}
	return false
}

// Managed reports whether the service is installed or a fallback process
// from the PID file is alive.
func (m *Manager) Managed() bool {
	if m.Installed() {
		return true
	}
	pid := m.readPID()
	return pid > 0 && processAlive(pid)
}

// Active reports whether the service manager says the service runs.
func (m *Manager) Active() bool {
	switch {
	case m.Platform == "macos" && exists(m.PlistPath):
		out, err := m.Runner.Run("launchctl", "list", m.Spec.Label)
		return err == nil && strings.TrimSpace(out) != ""
	case m.Platform == "linux" && exists(m.UnitPath):
		out, err := m.Runner.Run("systemctl", "--user", "is-active", m.Spec.Name)
		return err == nil && strings.TrimSpace(out) == "active"
	}
	pid := m.readPID()
	return pid > 0 && processAlive(pid)
}

// Install writes the service definition for the platform and starts it,
// falling back to a detached process where no service manager is available.
func (m *Manager) Install() error {
	if err := os.MkdirAll(m.Spec.LogDir, 0o700); err != nil {
		return err
	}
	switch {
	case m.Platform == "macos" && m.Runner.Exists("launchctl"):
		return m.installMac()
	case m.Platform == "linux" && m.Runner.Exists("systemctl"):
		if err := m.installLinux(); err == nil {
			return nil
		} else {
			m.log("warn", fmt.Sprintf("systemd setup failed (%s); falling back to detached process", err))
		}
	case m.Platform == "windows":
		return m.installWindows()
	}
	return m.StartFallback()
}

func (m *Manager) installMac() error {
	if err := envfile.WriteAtomic(m.PlistPath, []byte(RenderLaunchAgent(m.Spec)), 0o644); err != nil {
		return err
	}
	return m.loadMac()
}

// loadMac (re)loads the agent from the plist as it is on disk.
func (m *Manager) loadMac() error {
	if d := m.domain(); d != "" {
		_, _ = m.Runner.Run("launchctl", "bootout", d, m.PlistPath)
		if _, err := m.Runner.Run("launchctl", "bootstrap", d, m.PlistPath); err != nil {
			_, _ = m.Runner.Run("launchctl", "unload", m.PlistPath)
			if _, err := m.Runner.Run("launchctl", "load", m.PlistPath); err != nil {
				return err
			}
		} else {
			_, _ = m.Runner.Run("launchctl", "enable", m.target())
			_, _ = m.Runner.Run("launchctl", "kickstart", "-k", m.target())
		}
	} else {
		_, _ = m.Runner.Run("launchctl", "unload", m.PlistPath)
		if _, err := m.Runner.Run("launchctl", "load", m.PlistPath); err != nil {
			return err
		}
	}
	m.log("ok", fmt.Sprintf("launchd service loaded (%s)", m.Spec.Label))
	return nil
}

func (m *Manager) installLinux() error {
	if err := envfile.WriteAtomic(m.UnitPath, []byte(RenderSystemdUnit(m.Spec)), 0o644); err != nil {
		return err
	}
	for _, args := range [][]string{
		{"--user", "daemon-reload"},
		{"--user", "enable", m.Spec.Name},
		{"--user", "restart", m.Spec.Name},
		{"--user", "is-active", "--quiet", m.Spec.Name},
	} {
		if _, err := m.Runner.Run("systemctl", args...); err != nil {
			return err
		}
	}
	m.log("ok", "systemd user service installed and started")
	return nil
}

func (m *Manager) installWindows() error {
	if !m.Runner.Exists("schtasks") {
		m.log("warn", "Windows Task Scheduler is unavailable; using the background-process fallback.")
		return m.StartFallback()
	}
	command := quoteWindows(m.Spec.Program)
	for _, a := range m.Spec.Args {
		command += " " + quoteWindows(a)
	}
	if _, err := m.Runner.Run("schtasks", "/Create", "/TN", m.Spec.TaskName, "/TR", command, "/SC", "ONLOGON", "/RL", "LIMITED", "/F"); err != nil {
		m.log("warn", "Windows per-user autostart could not be registered; using the background-process fallback.")
		return m.StartFallback()
	}
	if err := m.StartFallback(); err != nil {
		return err
	}
	m.log("ok", "Windows per-user autostart registered")
	return nil
}

func quoteWindows(s string) string { return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"` }

// Launch starts the service from its definition as it is on disk (or the
// fallback process); it never rewrites the definition.
func (m *Manager) Launch() error {
	switch {
	case m.Platform == "macos" && exists(m.PlistPath):
		return m.loadMac()
	case m.Platform == "linux" && exists(m.UnitPath):
		if _, err := m.Runner.Run("systemctl", "--user", "daemon-reload"); err != nil {
			return err
		}
		if _, err := m.Runner.Run("systemctl", "--user", "start", m.Spec.Name); err != nil {
			return err
		}
		_, err := m.Runner.Run("systemctl", "--user", "is-active", "--quiet", m.Spec.Name)
		return err
	}
	return m.StartFallback()
}

// Stop asks the service manager, or the fallback process, to stop.
func (m *Manager) Stop() {
	switch {
	case m.Platform == "macos" && exists(m.PlistPath):
		if d := m.domain(); d != "" {
			_, _ = m.Runner.Run("launchctl", "bootout", d, m.PlistPath)
			_, _ = m.Runner.Run("launchctl", "bootout", m.target())
		}
		_, _ = m.Runner.Run("launchctl", "unload", m.PlistPath)
	case m.Platform == "linux" && exists(m.UnitPath):
		_, _ = m.Runner.Run("systemctl", "--user", "stop", m.Spec.Name)
	default:
		if pid := m.readPID(); pid > 0 {
			terminate(pid)
		}
		if m.Spec.PIDFile != "" {
			_ = os.Remove(m.Spec.PIDFile)
		}
	}
}

// Uninstall stops the service and removes its definition. Data is untouched.
func (m *Manager) Uninstall() {
	switch m.Platform {
	case "macos":
		m.Stop()
		_ = os.Remove(m.PlistPath)
	case "linux":
		_, _ = m.Runner.Run("systemctl", "--user", "stop", m.Spec.Name)
		_, _ = m.Runner.Run("systemctl", "--user", "disable", m.Spec.Name)
		_ = os.Remove(m.UnitPath)
		_, _ = m.Runner.Run("systemctl", "--user", "daemon-reload")
	case "windows":
		m.Stop()
		if m.Runner.Exists("schtasks") {
			_, _ = m.Runner.Run("schtasks", "/Delete", "/TN", m.Spec.TaskName, "/F")
		}
	default:
		m.Stop()
	}
}

// StartFallback starts the program as a detached background process and
// records its PID.
func (m *Manager) StartFallback() error {
	if err := os.MkdirAll(m.Spec.LogDir, 0o700); err != nil {
		return err
	}
	out, err := os.OpenFile(m.logFile(".log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer out.Close()
	errf, err := os.OpenFile(m.logFile(".error.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer errf.Close()
	cmd := exec.Command(m.Spec.Program, m.Spec.Args...)
	cmd.Dir = m.Spec.WorkDir
	cmd.Stdout, cmd.Stderr = out, errf
	cmd.Env = os.Environ()
	for _, e := range m.Spec.Env {
		cmd.Env = append(cmd.Env, e.Key+"="+e.Value)
	}
	cmd.SysProcAttr = detachedAttrs()
	if err := cmd.Start(); err != nil {
		return err
	}
	pid := cmd.Process.Pid
	_ = cmd.Process.Release()
	if m.Spec.PIDFile != "" {
		if err := envfile.WriteAtomic(m.Spec.PIDFile, []byte(strconv.Itoa(pid)), 0o600); err != nil {
			return err
		}
	}
	m.log("ok", fmt.Sprintf("Started detached process (pid %d)", pid))
	return nil
}

func (m *Manager) readPID() int {
	if m.Spec.PIDFile == "" {
		return 0
	}
	raw, err := os.ReadFile(m.Spec.PIDFile)
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 0 {
		return 0
	}
	return pid
}
