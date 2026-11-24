package runtimecfg

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveDefaultsAndOverrides(t *testing.T) {
	env := map[string]string{}
	get := func(k string) string { return env[k] }
	p := Resolve("NEOAGENT", ".neoagent", get, "/home/u")
	if p.Home != "/home/u/.neoagent" || p.DataDir != "/home/u/.neoagent/data" ||
		p.AgentDataDir != "/home/u/.neoagent/agent-data" || p.LogDir != "/home/u/.neoagent/data/logs" ||
		p.EnvFile != "/home/u/.neoagent/.env" {
		t.Fatalf("defaults: %+v", p)
	}
	env["NEOAGENT_HOME"] = "/srv/na"
	env["NEOAGENT_DATA_DIR"] = "/var/na"
	p = Resolve("NEOAGENT", ".neoagent", get, "/home/u")
	if p.Home != "/srv/na" || p.DataDir != "/var/na" || p.LogDir != "/var/na/logs" || p.EnvFile != "/srv/na/.env" {
		t.Fatalf("overrides: %+v", p)
	}
}

func TestEnsureCreatesPrivateDirs(t *testing.T) {
	root := t.TempDir()
	p := Resolve("X", ".x", func(string) string { return "" }, root)
	if err := p.Ensure(); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{p.Home, p.DataDir, p.LogDir, p.AgentDataDir} {
		st, err := os.Stat(d)
		if err != nil || st.Mode().Perm() != 0o700 {
			t.Fatalf("%s: %v %v", d, st, err)
		}
	}
}

func TestWritePrivateFileMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret.key")
	if err := WritePrivateFile(path, []byte("k")); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(path)
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", st.Mode().Perm())
	}
	if err := EnsurePrivateFile(filepath.Join(t.TempDir(), "missing")); err != nil {
		t.Fatalf("missing file must be fine: %v", err)
	}
}

func TestParseEnvAndLoadWithoutOverride(t *testing.T) {
	got := ParseEnv("# c\n\nA=1\nexport B=\"two words\"\nC='x'\nBAD\n=nokey\nD = spaced \n")
	want := map[string]string{"A": "1", "B": "two words", "C": "x", "D": "spaced"}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("%s: %q", k, got[k])
		}
	}

	file := filepath.Join(t.TempDir(), ".env")
	os.WriteFile(file, []byte("KEEP=file\nNEW=file\n"), 0o600)
	env := map[string]string{"KEEP": "real"}
	err := LoadEnvFile(file,
		func(k string) (string, bool) { v, ok := env[k]; return v, ok },
		func(k, v string) error { env[k] = v; return nil })
	if err != nil || env["KEEP"] != "real" || env["NEW"] != "file" {
		t.Fatalf("%v %v", env, err)
	}
	if err := LoadEnvFile(filepath.Join(t.TempDir(), "nope"), nil, nil); err != nil {
		t.Fatal("missing .env is not an error")
	}
}

func TestBool(t *testing.T) {
	for in, want := range map[string]bool{"1": true, "TRUE": true, " yes ": true, "on": true, "0": false, "No": false, "off": false} {
		if Bool(in, !want) != want {
			t.Errorf("%q", in)
		}
	}
	if !Bool("", true) || Bool("maybe", false) {
		t.Error("fallback")
	}
}
