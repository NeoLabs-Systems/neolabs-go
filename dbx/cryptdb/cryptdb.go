// Package cryptdb is dbx with the database encrypted at rest: the same writer
// and reader pools and migrations, on a pure Go SQLite whose pages, journals
// and WAL are sealed with Adiantum. Apps that need it import it; the others
// keep dbx and its smaller driver.
package cryptdb

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/NeoLabs-Systems/neolabs-go/dbx"
	"github.com/ncruces/go-sqlite3"
	_ "github.com/ncruces/go-sqlite3/driver" // database/sql driver "sqlite3"
	"github.com/ncruces/go-sqlite3/ext/fts5"
	_ "github.com/ncruces/go-sqlite3/vfs/adiantum" // the encrypting VFS
)

func init() {
	sqlite3.AutoExtension(fts5.Register)
}

const (
	driverName = "sqlite3"
	cipherVFS  = "adiantum"
	plainVFS   = "os"
)

var plainHeader = []byte("SQLite format 3\x00")

// ErrKey means the database cannot be read with this key.
var ErrKey = errors.New("cryptdb: the database cannot be opened with this key")

// ErrInUse means a plaintext database cannot be encrypted right now.
var ErrInUse = errors.New("cryptdb: the database is in use; stop the app before it is encrypted")

// pageKey separates the database key from the app's other uses of its secret.
func pageKey(secret []byte) (string, error) {
	if len(secret) < 32 {
		return "", errors.New("cryptdb: the key must be at least 32 bytes")
	}
	sum := sha256.Sum256(append([]byte("neolabs cryptdb v1\x00"), secret...))
	return hex.EncodeToString(sum[:]), nil
}

func fileURI(path string, query url.Values) string {
	return "file:" + (&url.URL{Path: filepath.ToSlash(path)}).EscapedPath() + "?" + query.Encode()
}

func encryptedDSN(path, hexKey string, pragmas ...string) string {
	q := url.Values{"vfs": {cipherVFS}}
	// The key has to be the first pragma: nothing can be read before it.
	q.Add("_pragma", "hexkey('"+hexKey+"')")
	for _, p := range pragmas {
		q.Add("_pragma", p)
	}
	return fileURI(path, q)
}

// IsPlaintext reports whether path is an unencrypted SQLite database. A missing
// or empty file is not.
func IsPlaintext(path string) (bool, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer f.Close()
	header := make([]byte, len(plainHeader))
	if _, err := io.ReadFull(f, header); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return false, nil
		}
		return false, err
	}
	return bytes.Equal(header, plainHeader), nil
}

// Open is dbx.Open for an encrypted database. A plaintext database already at
// path (an earlier build, or a restored backup) is encrypted first; nothing else
// may have it open while that happens.
func Open(ctx context.Context, path string, secret []byte, o dbx.Options) (*dbx.DB, error) {
	key, err := pageKey(secret)
	if err != nil {
		return nil, err
	}
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
		return nil, fmt.Errorf("cryptdb: create dir: %w", err)
	}
	if plain, err := IsPlaintext(path); err != nil {
		return nil, err
	} else if plain {
		if err := Encrypt(ctx, path, secret); err != nil {
			return nil, err
		}
	}
	if info, err := os.Stat(path); err == nil && info.Size() > 0 && !Readable(ctx, path, secret) {
		return nil, ErrKey
	}
	busy := fmt.Sprintf("busy_timeout(%d)", o.BusyTimeout.Milliseconds())

	w, err := sql.Open(driverName, encryptedDSN(path, key, busy, "journal_mode(WAL)", "foreign_keys(1)", "synchronous("+o.Synchronous+")", "temp_store(memory)"))
	if err != nil {
		return nil, fmt.Errorf("cryptdb: open writer: %w", err)
	}
	w.SetMaxOpenConns(1)
	// Reading the schema is what proves the key.
	var tables int
	if err := w.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_schema`).Scan(&tables); err != nil {
		w.Close()
		return nil, fmt.Errorf("%w: %w", ErrKey, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		w.Close()
		return nil, fmt.Errorf("cryptdb: chmod database: %w", err)
	}
	r, err := sql.Open(driverName, encryptedDSN(path, key, busy, "foreign_keys(1)", "query_only(1)", "temp_store(memory)"))
	if err != nil {
		w.Close()
		return nil, fmt.Errorf("cryptdb: open reader: %w", err)
	}
	r.SetMaxOpenConns(o.ReadConns)
	return &dbx.DB{W: w, R: r, Path: path}, nil
}

// Encrypt replaces the plaintext database at path with an encrypted copy. The
// plaintext stays until the copy has been checked.
func Encrypt(ctx context.Context, path string, secret []byte) error {
	key, err := pageKey(secret)
	if err != nil {
		return err
	}
	source, err := sql.Open(driverName, fileURI(path, url.Values{"vfs": {plainVFS}, "_pragma": {"busy_timeout(5000)"}}))
	if err != nil {
		return fmt.Errorf("cryptdb: open plaintext database: %w", err)
	}
	defer source.Close()
	source.SetMaxOpenConns(1)
	// Leaving WAL mode folds the log into the database and is only possible
	// while nothing else has it open, such as a server still running.
	var mode string
	if err := source.QueryRowContext(ctx, `PRAGMA journal_mode = DELETE`).Scan(&mode); err != nil || mode != "delete" {
		return fmt.Errorf("%w (journal mode %q: %w)", ErrInUse, mode, err)
	}
	var version int
	if err := source.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	next := path + ".encrypting"
	if err := os.Remove(next); err != nil && !os.IsNotExist(err) {
		return err
	}
	defer os.Remove(next)
	target := fileURI(next, url.Values{"vfs": {cipherVFS}, "hexkey": {key}})
	if _, err := source.ExecContext(ctx, `VACUUM INTO ?`, target); err != nil {
		return fmt.Errorf("cryptdb: write encrypted database: %w", err)
	}
	if err := verify(ctx, next, key, version); err != nil {
		return err
	}
	if err := source.Close(); err != nil {
		return err
	}
	for _, sidecar := range []string{path + "-wal", path + "-shm"} {
		if err := os.Remove(sidecar); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if err := os.Chmod(next, 0o600); err != nil {
		return err
	}
	return os.Rename(next, path)
}

func verify(ctx context.Context, path, key string, version int) error {
	db, err := sql.Open(driverName, encryptedDSN(path, key))
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	var result string
	if err := db.QueryRowContext(ctx, `PRAGMA quick_check`).Scan(&result); err != nil || result != "ok" {
		return fmt.Errorf("cryptdb: the encrypted copy failed its check (%s): %w", result, err)
	}
	if _, err := db.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version = %d`, version)); err != nil {
		return err
	}
	return nil
}

// Export writes an unencrypted copy of an open database to path, for backups
// that are sealed afterwards. path must not exist.
func Export(ctx context.Context, db *sql.DB, path string) error {
	_, err := db.ExecContext(ctx, `VACUUM INTO ?`, fileURI(path, url.Values{"vfs": {plainVFS}}))
	return err
}

// pageSize is the size of the unit Adiantum seals.
const pageSize = 4096

// Readable reports whether path is a database encrypted with this key. It looks
// at a copy of the first page only: SQLite itself discards the log of a file it
// cannot read, so a wrong key must never reach the real one.
func Readable(ctx context.Context, path string, secret []byte) bool {
	key, err := pageKey(secret)
	if err != nil {
		return false
	}
	source, err := os.Open(path)
	if err != nil {
		return false
	}
	defer source.Close()
	first := make([]byte, pageSize)
	if _, err := io.ReadFull(source, first); err != nil {
		return false
	}
	dir, err := os.MkdirTemp("", "cryptdb-check-")
	if err != nil {
		return false
	}
	defer os.RemoveAll(dir)
	probe := filepath.Join(dir, "page.db")
	if err := os.WriteFile(probe, first, 0o600); err != nil {
		return false
	}
	db, err := sql.Open(driverName, encryptedDSN(probe, key))
	if err != nil {
		return false
	}
	defer db.Close()
	var version int
	err = db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version)
	// A wrong key turns the header into noise, which SQLite calls "not a
	// database". The right key may still complain that the copy is cut short.
	return !errors.Is(err, sqlite3.NOTADB)
}
