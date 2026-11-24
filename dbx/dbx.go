// Package dbx opens the SQLite database every NeoLabs app uses and applies
// schema migrations. SQLite runs in WAL mode with one serialised writer
// connection and a pool of read-only connections, which keeps writes safe and
// reads concurrent. Migrations are tracked in PRAGMA user_version, the same
// bookkeeping the Node servers used, so existing databases are picked up as-is.
package dbx

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver, FTS5 and JSON1 included
)

// Options tune Open.
type Options struct {
	Synchronous string        // FULL (default) or NORMAL
	BusyTimeout time.Duration // default 5s
	ReadConns   int           // default 4
}

// DB is a writer pool (one connection) and a reader pool.
type DB struct {
	W    *sql.DB
	R    *sql.DB
	Path string
}

func dsn(path string, pragmas ...string) string {
	q := url.Values{}
	for _, p := range pragmas {
		q.Add("_pragma", p)
	}
	return "file:" + path + "?" + q.Encode()
}

// Open opens (creating if needed) the database at path with private file
// permissions and the standard pragmas.
func Open(ctx context.Context, path string, o Options) (*DB, error) {
	if o.Synchronous == "" {
		o.Synchronous = "FULL"
	}
	if o.BusyTimeout == 0 {
		o.BusyTimeout = 5 * time.Second
	}
	if o.ReadConns == 0 {
		o.ReadConns = 4
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("dbx: create dir: %w", err)
	}
	busy := fmt.Sprintf("busy_timeout(%d)", o.BusyTimeout.Milliseconds())

	w, err := sql.Open("sqlite", dsn(path, busy, "journal_mode(WAL)", "foreign_keys(1)", "synchronous("+o.Synchronous+")"))
	if err != nil {
		return nil, fmt.Errorf("dbx: open writer: %w", err)
	}
	w.SetMaxOpenConns(1)
	if err := w.PingContext(ctx); err != nil {
		w.Close()
		return nil, fmt.Errorf("dbx: connect writer: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		w.Close()
		return nil, fmt.Errorf("dbx: chmod database: %w", err)
	}

	r, err := sql.Open("sqlite", dsn(path, busy, "foreign_keys(1)", "query_only(1)"))
	if err != nil {
		w.Close()
		return nil, fmt.Errorf("dbx: open reader: %w", err)
	}
	r.SetMaxOpenConns(o.ReadConns)
	return &DB{W: w, R: r, Path: path}, nil
}

// Close closes both pools.
func (d *DB) Close() error {
	errR := d.R.Close()
	if err := d.W.Close(); err != nil {
		return err
	}
	return errR
}

// Step is one ordered, one-shot migration. It runs inside a transaction.
type Step func(ctx context.Context, tx *sql.Tx) error

// Migrate applies the steps after the database's recorded version, each in its
// own transaction, and records progress in PRAGMA user_version. A database that
// is newer than this binary is refused rather than guessed at.
func Migrate(ctx context.Context, db *sql.DB, steps []Step) error {
	var applied int
	if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&applied); err != nil {
		return fmt.Errorf("dbx: read user_version: %w", err)
	}
	if applied > len(steps) {
		return fmt.Errorf("dbx: database schema version %d is newer than this build supports (%d); upgrade the app", applied, len(steps))
	}
	for i := applied; i < len(steps); i++ {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("dbx: begin migration %d: %w", i+1, err)
		}
		if err := steps[i](ctx, tx); err != nil {
			tx.Rollback()
			return fmt.Errorf("dbx: migration %d: %w", i+1, err)
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version = %d`, i+1)); err != nil {
			tx.Rollback()
			return fmt.Errorf("dbx: record migration %d: %w", i+1, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("dbx: commit migration %d: %w", i+1, err)
		}
	}
	return nil
}
