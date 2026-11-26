package cryptdb

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/NeoLabs-Systems/neolabs-go/dbx"
)

var secret = bytes.Repeat([]byte{7}, 32)

const marker = "a sentence nobody else may read"

func onDisk(t *testing.T, path string) []byte {
	t.Helper()
	var all []byte
	for _, p := range []string{path, path + "-wal"} {
		raw, err := os.ReadFile(p)
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		all = append(all, raw...)
	}
	return all
}

func fill(t *testing.T, db *dbx.DB) {
	t.Helper()
	ctx := context.Background()
	for _, q := range []string{
		`CREATE TABLE t(id INTEGER PRIMARY KEY, body TEXT, at DATETIME)`,
		`CREATE VIRTUAL TABLE f USING fts5(body)`,
		`INSERT INTO t(body, at) VALUES('` + marker + `', '2026-11-26 10:00:00')`,
		`INSERT INTO f VALUES('` + marker + `')`,
		`PRAGMA user_version = 7`,
	} {
		if _, err := db.W.ExecContext(ctx, q); err != nil {
			t.Fatal(q, err)
		}
	}
}

func check(t *testing.T, db *dbx.DB) {
	t.Helper()
	var body, at string
	var hits, version int
	ctx := context.Background()
	if err := db.R.QueryRowContext(ctx, `SELECT body, at FROM t`).Scan(&body, &at); err != nil || body != marker || at != "2026-11-26 10:00:00" {
		t.Fatal(body, at, err)
	}
	if err := db.R.QueryRowContext(ctx, `SELECT count(*) FROM f WHERE f MATCH 'sentence'`).Scan(&hits); err != nil || hits != 1 {
		t.Fatal("full text search:", hits, err)
	}
	if err := db.R.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil || version != 7 {
		t.Fatal("user_version:", version, err)
	}
}

func TestNothingIsReadableOnDiskAndAWrongKeyIsRefused(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "data dir", "app.db")
	db, err := Open(ctx, path, secret, dbx.Options{})
	if err != nil {
		t.Fatal(err)
	}
	fill(t, db)
	check(t, db)
	if raw := onDisk(t, path); bytes.Contains(raw, []byte("nobody else")) || bytes.Contains(raw, plainHeader) {
		t.Fatal("plaintext on disk")
	}
	if _, err := db.R.ExecContext(ctx, `DELETE FROM t`); err == nil {
		t.Fatal("the read pool accepted a write")
	}
	export := filepath.Join(t.TempDir(), "export.db")
	if err := Export(ctx, db.W, export); err != nil {
		t.Fatal(err)
	}
	if plain, _ := IsPlaintext(export); !plain {
		t.Fatal("an export must be readable")
	}
	db.Close()
	if _, err := Open(ctx, path, bytes.Repeat([]byte{9}, 32), dbx.Options{}); !errors.Is(err, ErrKey) {
		t.Fatal("wrong key:", err)
	}
}

func TestAPlaintextDatabaseIsEncryptedWithItsContent(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "app.db")
	plain, err := dbx.Open(ctx, path, dbx.Options{})
	if err != nil {
		t.Fatal(err)
	}
	fill(t, plain)
	plain.Close()
	db, err := Open(ctx, path, secret, dbx.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	check(t, db)
	if raw := onDisk(t, path); bytes.Contains(raw, []byte("nobody else")) {
		t.Fatal("plaintext left behind")
	}
	if _, err := os.Stat(path + ".encrypting"); !os.IsNotExist(err) {
		t.Fatal("temporary copy left behind")
	}
}

func TestADatabaseSomeoneStillHasOpenIsLeftAlone(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "app.db")
	plain, err := dbx.Open(ctx, path, dbx.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer plain.Close()
	fill(t, plain)
	var n int
	if err := plain.R.QueryRowContext(ctx, `SELECT count(*) FROM t`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if err := Encrypt(ctx, path, secret); !errors.Is(err, ErrInUse) {
		t.Fatal("encrypted under an open database:", err)
	}
	if still, _ := IsPlaintext(path); !still {
		t.Fatal("the database was replaced")
	}
	var body string
	if err := plain.R.QueryRowContext(ctx, `SELECT body FROM t`).Scan(&body); err != nil || body != marker {
		t.Fatal(body, err)
	}
}

func TestAWrongKeyNeverTouchesTheDatabaseOrItsLog(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "app.db")
	db, err := Open(ctx, path, secret, dbx.Options{})
	if err != nil {
		t.Fatal(err)
	}
	fill(t, db)
	// A server that was killed leaves its changes in the log.
	snapshot := map[string][]byte{}
	for _, p := range []string{path, path + "-wal"} {
		raw, err := os.ReadFile(p)
		if err != nil || len(raw) == 0 {
			t.Fatal(p, len(raw), err)
		}
		snapshot[p] = raw
	}
	db.Close()
	for p, raw := range snapshot {
		if err := os.WriteFile(p, raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	wrong := bytes.Repeat([]byte{9}, 32)
	if Readable(ctx, path, wrong) {
		t.Fatal("readable with the wrong key")
	}
	if _, err := Open(ctx, path, wrong, dbx.Options{}); !errors.Is(err, ErrKey) {
		t.Fatal(err)
	}
	for p, raw := range snapshot {
		if got, err := os.ReadFile(p); err != nil || !bytes.Equal(got, raw) {
			t.Fatal("changed by a wrong key:", p, err)
		}
	}
	reopened, err := Open(ctx, path, secret, dbx.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	check(t, reopened)
}
