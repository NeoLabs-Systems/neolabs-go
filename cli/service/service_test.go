package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fakeRunner struct {
	calls  []string
	exists map[string]bool
	fail   map[string]bool
}

func (f *fakeRunner) Run(name string, args ...string) (string, error) {
	call := name + " " + strings.Join(args, " ")
	f.calls = append(f.calls, call)
	if f.fail[call] {
		return "", os.ErrPermission
	}
	return "active\n", nil
}

func (f *fakeRunner) Exists(name string) bool { return f.exists[name] }

func spec(dir string) Spec {
	return Spec{
		Label: "com.example", Name: "example", TaskName: "Example", Description: "Example app",
		Program: "/opt/ex & co/bin/example", Args: []string{"serve"}, WorkDir: "/opt/ex",
		Env:     []EnvVar{{"HOME", "/home/u"}, {"EXAMPLE_HOME", `/data/"x"`}},
		EnvFile: "/data/.env", LogDir: filepath.Join(dir, "logs"), LogName: "example",
		PIDFile: filepath.Join(dir, "example.pid"),
	}
}

func TestRenderers(t *testing.T) {
	s := spec("/tmp/d")
	plist := RenderLaunchAgent(s)
	for _, want := range []string{"<string>com.example</string>", "<string>/opt/ex &amp; co/bin/example</string>", "<string>serve</string>", "<key>EXAMPLE_HOME</key>\n        <string>/data/&quot;x&quot;</string>", "/tmp/d/logs/example.error.log"} {
		if !strings.Contains(plist, want) {
			t.Errorf("plist lacks %q:\n%s", want, plist)
		}
	}
	unit := RenderSystemdUnit(s)
	for _, want := range []string{`ExecStart="/opt/ex & co/bin/example" "serve"`, "EnvironmentFile=-/data/.env", `Environment=EXAMPLE_HOME="/data/\"x\""`, "StandardOutput=append:/tmp/d/logs/example.log", "WantedBy=default.target\n"} {
		if !strings.Contains(unit, want) {
			t.Errorf("unit lacks %q:\n%s", want, unit)
		}
	}
}

func TestLinuxInstallAndUninstallUseTempPaths(t *testing.T) {
	dir := t.TempDir()
	r := &fakeRunner{exists: map[string]bool{"systemctl": true}}
	m := &Manager{Spec: spec(dir), Platform: "linux", Runner: r, UnitPath: filepath.Join(dir, "unit.service"), PlistPath: filepath.Join(dir, "x.plist"), UID: 501}
	if err := m.Install(); err != nil {
		t.Fatal(err)
	}
	if !m.Installed() || !m.Active() {
		t.Fatal("not installed")
	}
	if got := strings.Join(r.calls[:4], "|"); got != "systemctl --user daemon-reload|systemctl --user enable example|systemctl --user restart example|systemctl --user is-active --quiet example" {
		t.Fatal(got)
	}
	m.Uninstall()
	if m.Installed() {
		t.Fatal("unit left behind")
	}
}

func TestMacInstallFallsBackToLoad(t *testing.T) {
	dir := t.TempDir()
	plist := filepath.Join(dir, "com.example.plist")
	r := &fakeRunner{exists: map[string]bool{"launchctl": true}, fail: map[string]bool{"launchctl bootstrap gui/501 " + plist: true}}
	m := &Manager{Spec: spec(dir), Platform: "macos", Runner: r, PlistPath: plist, UID: 501}
	if err := m.Install(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(r.calls, "|"), "launchctl load "+plist) {
		t.Fatal(r.calls)
	}
}

func TestFallbackProcessWritesPID(t *testing.T) {
	dir := t.TempDir()
	s := spec(dir)
	s.Program, s.Args, s.WorkDir = "/bin/sh", []string{"-c", "sleep 30"}, dir
	m := &Manager{Spec: s, Platform: "other", Runner: &fakeRunner{}}
	if err := m.Install(); err != nil {
		t.Fatal(err)
	}
	if !m.Managed() || !m.Active() {
		t.Fatal("fallback process not tracked")
	}
	m.Stop()
	if _, err := os.Stat(s.PIDFile); !os.IsNotExist(err) {
		t.Fatal("pid file left")
	}
}
