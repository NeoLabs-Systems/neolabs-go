// Package testkit holds helpers for tests across the NeoLabs apps. Tests must
// never touch a real data directory: opening a database can run migrations on
// it, so they work on temp dirs or on copies made here.
package testkit

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// CopySQLite makes a consistent standalone copy of a SQLite database (WAL
// included) with VACUUM INTO, never modifying the source. It skips the test if
// the source does not exist, so suites stay green on machines without a fixture.
func CopySQLite(t testing.TB, src string) string {
	t.Helper()
	if _, err := os.Stat(src); err != nil {
		t.Skipf("fixture database %s not available", src)
	}
	dst := filepath.Join(t.TempDir(), filepath.Base(src))
	q := url.Values{}
	q.Set("mode", "ro")
	db, err := sql.Open("sqlite", "file:"+src+"?"+q.Encode())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(context.Background(), `VACUUM INTO '`+strings.ReplaceAll(dst, "'", "''")+`'`); err != nil {
		t.Fatalf("copy %s: %v", src, err)
	}
	return dst
}

// Env returns a lookup function backed by a map, for config tests.
func Env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}
