package servicemail

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/NeoLabs-Systems/neolabs-go/passhash"
	"github.com/NeoLabs-Systems/neolabs-go/userid"
)

const tokenBytes = 32

// Tokens are the single-use links in account emails. Only the SHA-256 of a
// token is stored, so a leaked database does not leak usable links. They live
// in the table
//
//	user_email_tokens(id, user_id, type, email, token_hash UNIQUE, created_at, expires_at, consumed_at)
type Tokens struct {
	DB  *sql.DB // writer
	Now func() time.Time
}

func (t Tokens) now() time.Time {
	if t.Now != nil {
		return t.Now()
	}
	return time.Now()
}

// SQLiteTime formats a time the way datetime('now') does (UTC, no zone).
func SQLiteTime(tm time.Time) string { return tm.UTC().Format("2006-01-02 15:04:05") }

// Create issues a token and retires earlier unused tokens of the same type for
// the account, so only the latest link works.
func (t Tokens) Create(ctx context.Context, userID userid.ID, typ, email string, ttl time.Duration) (token, expiresAt string, err error) {
	buf := make([]byte, tokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", "", fmt.Errorf("servicemail: random token: %w", err)
	}
	token = base64.RawURLEncoding.EncodeToString(buf)
	expiresAt = SQLiteTime(t.now().Add(ttl))
	tx, err := t.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", "", err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE user_email_tokens SET consumed_at = COALESCE(consumed_at, datetime('now'))
		WHERE user_id = ? AND type = ? AND consumed_at IS NULL`, string(userID), typ); err != nil {
		return "", "", err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO user_email_tokens (user_id, type, email, token_hash, expires_at) VALUES (?, ?, ?, ?, ?)`,
		string(userID), typ, email, passhash.DigestToken(token), expiresAt); err != nil {
		return "", "", err
	}
	return token, expiresAt, tx.Commit()
}

// Row is a live token with the account it belongs to.
type Row struct {
	ID           int64
	UserID       userid.ID
	Type         string
	Email        string
	Username     string
	CurrentEmail sql.NullString
}

// Find returns the live (unused, unexpired) token row. typ "" accepts any type.
func (t Tokens) Find(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, token, typ string) (*Row, error) {
	var r Row
	err := q.QueryRowContext(ctx, `
		SELECT t.id, t.user_id, t.type, t.email, u.username, u.email
		FROM user_email_tokens t JOIN users u ON u.id = t.user_id
		WHERE t.token_hash = ? AND (? = '' OR t.type = ?)
		  AND t.consumed_at IS NULL AND datetime(t.expires_at) > datetime('now')`,
		passhash.DigestToken(token), typ, typ).Scan(&r.ID, &r.UserID, &r.Type, &r.Email, &r.Username, &r.CurrentEmail)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// AttemptLimiter caps wrong password-reset tries per token (12 per 15 minutes),
// so a link cannot be brute-forced even from many addresses.
type AttemptLimiter struct {
	Window time.Duration
	Max    int
	Now    func() time.Time

	mu    sync.Mutex
	state map[string]*attempt
}

type attempt struct {
	started time.Time
	count   int
}

// ErrTooManyAttempts is returned while a token is locked.
var ErrTooManyAttempts = errors.New("Too many password reset attempts. Try again later.")

func (l *AttemptLimiter) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now()
}

func (l *AttemptLimiter) window() time.Duration {
	if l.Window > 0 {
		return l.Window
	}
	return 15 * time.Minute
}

func (l *AttemptLimiter) max() int {
	if l.Max > 0 {
		return l.Max
	}
	return 12
}

// Allowed returns ErrTooManyAttempts when the key is locked.
func (l *AttemptLimiter) Allowed(key string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	cur := l.state[key]
	if cur == nil {
		return nil
	}
	if l.now().Sub(cur.started) > l.window() {
		delete(l.state, key)
		return nil
	}
	if cur.count >= l.max() {
		return ErrTooManyAttempts
	}
	return nil
}

// Register counts a failed try.
func (l *AttemptLimiter) Register(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.state == nil {
		l.state = map[string]*attempt{}
	}
	now := l.now()
	if cur := l.state[key]; cur != nil && now.Sub(cur.started) <= l.window() {
		cur.count++
		return
	}
	l.state[key] = &attempt{started: now, count: 1}
}

// Reset forgets the key after a success.
func (l *AttemptLimiter) Reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.state, key)
}

// Execer runs a statement; *sql.DB and *sql.Tx satisfy it.
type Execer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

// ErrTokenSpent is returned when a token was used, expired or retired between
// finding it and spending it.
var ErrTokenSpent = errors.New("servicemail: token already used or expired")

// Consume spends the token with one write that only succeeds while it is
// live, so two requests racing on the same link cannot both act on it. Run it
// in the transaction that applies the token's effect.
func (t Tokens) Consume(ctx context.Context, q Execer, id int64) error {
	res, err := q.ExecContext(ctx, `UPDATE user_email_tokens SET consumed_at = datetime('now')
		WHERE id = ? AND consumed_at IS NULL AND datetime(expires_at) > datetime('now')`, id)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		if err != nil {
			return err
		}
		return ErrTokenSpent
	}
	return nil
}

// Retire spends every unused token of the given types for the account; no
// types retires them all.
func (t Tokens) Retire(ctx context.Context, q Execer, userID userid.ID, types ...string) error {
	query := `UPDATE user_email_tokens SET consumed_at = datetime('now') WHERE user_id = ? AND consumed_at IS NULL`
	args := []any{string(userID)}
	if len(types) > 0 {
		query += ` AND type IN (?` + strings.Repeat(`, ?`, len(types)-1) + `)`
		for _, typ := range types {
			args = append(args, typ)
		}
	}
	_, err := q.ExecContext(ctx, query, args...)
	return err
}

// Recent counts the tokens of a type issued to the account since the given
// time, used or not, to cap how much mail one account can trigger.
func (t Tokens) Recent(ctx context.Context, userID userid.ID, typ string, since time.Time) (int, error) {
	var n int
	err := t.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM user_email_tokens
		WHERE user_id = ? AND type = ? AND datetime(created_at) >= datetime(?)`,
		string(userID), typ, SQLiteTime(since)).Scan(&n)
	return n, err
}

// Pending is the address of the account's newest live token of a type, such
// as an email change waiting for confirmation; "" when there is none.
func (t Tokens) Pending(ctx context.Context, userID userid.ID, typ string) (string, error) {
	var email string
	err := t.DB.QueryRowContext(ctx, `SELECT email FROM user_email_tokens
		WHERE user_id = ? AND type = ? AND consumed_at IS NULL AND datetime(expires_at) > datetime('now')
		ORDER BY id DESC LIMIT 1`, string(userID), typ).Scan(&email)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return email, err
}
