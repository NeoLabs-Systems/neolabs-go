package dbx

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func open(t *testing.T) *DB {
	t.Helper()
	d, err := Open(context.Background(), filepath.Join(t.TempDir(), "sub", "t.db"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func TestOpenSetsPragmasAndPermissions(t *testing.T) {
	d := open(t)
	ctx := context.Background()
	var mode string
	d.W.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&mode)
	if mode != "wal" {
		t.Fatalf("journal_mode %q", mode)
	}
	var fk int
	d.W.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&fk)
	if fk != 1 {
		t.Fatal("foreign keys off")
	}
	var sync int
	d.W.QueryRowContext(ctx, `PRAGMA synchronous`).Scan(&sync)
	if sync != 2 { // FULL
		t.Fatalf("synchronous %d", sync)
	}
	st, _ := os.Stat(d.Path)
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("db mode %v", st.Mode().Perm())
	}
	if dir, _ := os.Stat(filepath.Dir(d.Path)); dir.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode %v", dir.Mode().Perm())
	}
}

func TestReaderCannotWrite(t *testing.T) {
	d := open(t)
	ctx := context.Background()
	if _, err := d.W.ExecContext(ctx, `CREATE TABLE t (id INTEGER)`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.R.ExecContext(ctx, `INSERT INTO t VALUES (1)`); err == nil {
		t.Fatal("read pool accepted a write")
	}
	if _, err := d.W.ExecContext(ctx, `INSERT INTO t VALUES (1)`); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := d.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM t`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("reader sees %d (%v)", n, err)
	}
}

func TestFTS5AndJSONAvailable(t *testing.T) {
	d := open(t)
	ctx := context.Background()
	if _, err := d.W.ExecContext(ctx, `CREATE VIRTUAL TABLE f USING fts5(body)`); err != nil {
		t.Fatalf("fts5: %v", err)
	}
	d.W.ExecContext(ctx, `INSERT INTO f VALUES ('hello world')`)
	var n int
	d.W.QueryRowContext(ctx, `SELECT COUNT(*) FROM f WHERE f MATCH 'hello'`).Scan(&n)
	var js string
	d.W.QueryRowContext(ctx, `SELECT json_extract('{"a":1}', '$.a')`).Scan(&js)
	if n != 1 || js != "1" {
		t.Fatalf("n=%d js=%q", n, js)
	}
}

func TestMigrateAppliesOnceAndInOrder(t *testing.T) {
	d := open(t)
	ctx := context.Background()
	var ran []int
	steps := []Step{
		func(ctx context.Context, tx *sql.Tx) error {
			ran = append(ran, 1)
			_, err := tx.ExecContext(ctx, `CREATE TABLE a (x)`)
			return err
		},
		func(ctx context.Context, tx *sql.Tx) error {
			ran = append(ran, 2)
			_, err := tx.ExecContext(ctx, `CREATE TABLE b (x)`)
			return err
		},
	}
	if err := Migrate(ctx, d.W, steps); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, d.W, steps); err != nil {
		t.Fatal(err)
	}
	if len(ran) != 2 || ran[0] != 1 || ran[1] != 2 {
		t.Fatalf("ran %v", ran)
	}
	var v int
	d.W.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&v)
	if v != 2 {
		t.Fatalf("user_version %d", v)
	}
}

func TestFailedMigrationRollsBackAndStops(t *testing.T) {
	d := open(t)
	ctx := context.Background()
	boom := errors.New("boom")
	steps := []Step{
		func(ctx context.Context, tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `CREATE TABLE ok (x)`)
			return err
		},
		func(ctx context.Context, tx *sql.Tx) error {
			tx.ExecContext(ctx, `CREATE TABLE half (x)`)
			return boom
		},
		func(ctx context.Context, tx *sql.Tx) error { t.Fatal("must not run after a failure"); return nil },
	}
	err := Migrate(ctx, d.W, steps)
	if !errors.Is(err, boom) || !strings.Contains(err.Error(), "migration 2") {
		t.Fatalf("err %v", err)
	}
	var v, half int
	d.W.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&v)
	d.W.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE name='half'`).Scan(&half)
	if v != 1 || half != 0 {
		t.Fatalf("version %d, half-applied tables %d", v, half)
	}
}

func TestNewerDatabaseIsRefused(t *testing.T) {
	d := open(t)
	ctx := context.Background()
	d.W.ExecContext(ctx, `PRAGMA user_version = 9`)
	if err := Migrate(ctx, d.W, make([]Step, 3)); err == nil || !strings.Contains(err.Error(), "newer") {
		t.Fatalf("err %v", err)
	}
}
