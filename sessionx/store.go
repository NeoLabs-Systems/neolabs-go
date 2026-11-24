package sessionx

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Store keeps sessions in the `sessions(sid, sess, expire)` table used by
// better-sqlite3-session-store. `expire` is an ISO-8601 UTC timestamp.
type Store struct {
	db *sql.DB
}

// NewStore creates the table if needed. An existing table with the same
// columns (created by the Node server) is used as it is.
func NewStore(ctx context.Context, db *sql.DB) (*Store, error) {
	_, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS sessions
		(sid TEXT NOT NULL PRIMARY KEY, sess JSON NOT NULL, expire TEXT NOT NULL)`)
	if err != nil {
		return nil, fmt.Errorf("sessionx: create table: %w", err)
	}
	return &Store{db: db}, nil
}

// Get returns the stored session data, or nil when it is missing or expired.
// Numbers decode as json.Number so ids keep their exact value.
func (s *Store) Get(ctx context.Context, sid string) (map[string]any, error) {
	var raw string
	err := s.db.QueryRowContext(ctx,
		`SELECT sess FROM sessions WHERE sid = ? AND datetime('now') < datetime(expire)`, sid).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("sessionx: get: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader([]byte(raw)))
	dec.UseNumber()
	var data map[string]any
	if err := dec.Decode(&data); err != nil {
		return nil, nil // corrupt row behaves like a missing session
	}
	return data, nil
}

// Set stores data until now+maxAge.
func (s *Store) Set(ctx context.Context, sid string, data map[string]any, maxAge time.Duration) error {
	raw, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("sessionx: encode: %w", err)
	}
	expire := time.Now().Add(maxAge).UTC().Format("2006-01-02T15:04:05.000Z")
	_, err = s.db.ExecContext(ctx,
		`INSERT OR REPLACE INTO sessions (sid, sess, expire) VALUES (?, ?, ?)`, sid, string(raw), expire)
	if err != nil {
		return fmt.Errorf("sessionx: set: %w", err)
	}
	return nil
}

// Destroy removes one session.
func (s *Store) Destroy(ctx context.Context, sid string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE sid = ?`, sid)
	return err
}

// ClearExpired removes sessions past their expiry.
func (s *Store) ClearExpired(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE datetime('now') > datetime(expire)`)
	return err
}

// DestroyWhere removes every session whose JSON matches a user id, used to log
// an account out everywhere (password change, admin revoke).
func (s *Store) DestroyForUser(ctx context.Context, userID string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM sessions WHERE CAST(json_extract(sess, '$.userId') AS TEXT) = ?`, userID)
	return err
}
