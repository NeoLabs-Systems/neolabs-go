package envfile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestUpsertRemoveAndPrivateMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", ".env")
	if err := Upsert(path, "PORT", "3333"); err != nil {
		t.Fatal(err)
	}
	if err := Upsert(path, "SECRET", "a\nINJECT=1"); err != nil {
		t.Fatal(err)
	}
	if err := Upsert(path, "PORT", "4000"); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if string(raw) != "PORT=4000\nSECRET=aINJECT=1\n" {
		t.Fatalf("%q", raw)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", info.Mode())
	}
	if ok, err := Remove(path, "PORT"); !ok || err != nil {
		t.Fatal(ok, err)
	}
	raw, _ = os.ReadFile(path)
	if string(raw) != "SECRET=aINJECT=1\n" {
		t.Fatalf("%q", raw)
	}
	if err := Upsert(path, "bad key", "x"); err == nil {
		t.Fatal("invalid key accepted")
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Fatalf("temp files left: %v", entries)
	}
}

// Same result as Node's mergeEnvText for the same input.
func TestMergeMatchesNode(t *testing.T) {
	got := Merge("# c\nPORT=1\nA=x\nPORT=2\n\n\n", []string{"PORT", "NEW", "B"}, map[string]string{"PORT": "3", "NEW": "y", "B": "z"})
	if got != "# c\nPORT=3\nA=x\nNEW=y\nB=z\n" {
		t.Fatalf("%q", got)
	}
}

func TestParseAndMask(t *testing.T) {
	v := Parse("# x\r\n A =1=2\nEMPTY=\nnoequals\n")
	if v["A"] != "1=2" || v["EMPTY"] != "" || len(v) != 2 {
		t.Fatalf("%v", v)
	}
	if Mask("PORT", "3333") != "3333" || Mask("API_KEY", "short") != "********" || Mask("TOKEN", "abcdefghijkl") != "abcd...ijkl" {
		t.Fatal("mask")
	}
}
