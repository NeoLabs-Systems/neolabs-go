// Package lockout throttles sensitive confirmations (current password, 2FA
// code) per account, not per client address, so a stolen session or a client
// that rotates addresses cannot guess faster than the policy allows. From the
// 5th failure in 15 minutes each next try waits twice as long (2 s, 4 s, ...);
// the 10th locks confirmations until the oldest of those failures leaves the
// window. Failures are counted before the secret is checked, so parallel
// guesses count too; a success takes its failures back.
//
// It needs the table
//
//	login_failures(account_key TEXT, kind TEXT, failed_at_ms INTEGER)
package lockout

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"sync"
	"time"
)

const (
	window         = 15 * time.Minute
	failuresDelay  = 5
	failuresLock   = 10
	baseDelay      = 2 * time.Second
	retryAfterCode = "LOGIN_LOCKED"
)

// LockedError is returned while an account is throttled.
type LockedError struct {
	RetryAfter int // seconds
}

func (e *LockedError) Error() string {
	wait := fmt.Sprintf("%d seconds", e.RetryAfter)
	if e.RetryAfter >= 90 {
		wait = fmt.Sprintf("%d minutes", int(math.Ceil(float64(e.RetryAfter)/60)))
	}
	return "Too many wrong passwords or codes for this account. Try again in " + wait + "."
}

// Status is the HTTP status clients receive.
func (e *LockedError) Status() int { return 429 }

// Code is the stable error code clients branch on.
func (e *LockedError) Code() string { return retryAfterCode }

// Store reads and writes failures.
type Store struct {
	DB  *sql.DB
	Now func() time.Time
}

func (s Store) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// ConfirmKey is the key a signed-in account's confirmations count under.
func ConfirmKey(userID string) string { return "confirm:user:" + userID }

func locked(retry time.Duration) *LockedError {
	secs := int(math.Ceil(retry.Seconds()))
	if secs < 1 {
		secs = 1
	}
	return &LockedError{RetryAfter: secs}
}

// Check returns a *LockedError while the key is throttled.
func (s Store) Check(ctx context.Context, key string) error {
	now := s.now()
	nowMs := now.UnixMilli()
	rows, err := s.DB.QueryContext(ctx,
		`SELECT failed_at_ms FROM login_failures WHERE account_key = ? AND failed_at_ms > ? ORDER BY failed_at_ms`,
		key, nowMs-window.Milliseconds())
	if err != nil {
		return fmt.Errorf("lockout: read failures: %w", err)
	}
	defer rows.Close()
	var failures []int64
	for rows.Next() {
		var ms int64
		if err := rows.Scan(&ms); err != nil {
			return err
		}
		failures = append(failures, ms)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	n := len(failures)
	if n >= failuresLock {
		until := failures[n-failuresLock] + window.Milliseconds()
		return locked(time.Duration(until-nowMs) * time.Millisecond)
	}
	if n >= failuresDelay {
		next := failures[n-1] + baseDelay.Milliseconds()*(int64(1)<<(n-failuresDelay))
		if nowMs < next {
			return locked(time.Duration(next-nowMs) * time.Millisecond)
		}
	}
	return nil
}

// Record counts a try as failed and forgets failures that left the window.
func (s Store) Record(ctx context.Context, key, kind string) error {
	nowMs := s.now().UnixMilli()
	if _, err := s.DB.ExecContext(ctx, `DELETE FROM login_failures WHERE failed_at_ms <= ?`, nowMs-window.Milliseconds()); err != nil {
		return fmt.Errorf("lockout: prune: %w", err)
	}
	if _, err := s.DB.ExecContext(ctx,
		`INSERT INTO login_failures (account_key, kind, failed_at_ms) VALUES (?, ?, ?)`, key, kind, nowMs); err != nil {
		return fmt.Errorf("lockout: record: %w", err)
	}
	return nil
}

// attempts makes Attempt's check and record one step within the process.
var attempts sync.Mutex

// Attempt checks the key and, when it is not throttled, records the try as
// failed, as one step: parallel guesses cannot all pass the check before any
// of them is counted. Clear the kind once the secret proved right.
func (s Store) Attempt(ctx context.Context, key, kind string) error {
	attempts.Lock()
	defer attempts.Unlock()
	if err := s.Check(ctx, key); err != nil {
		return err
	}
	return s.Record(ctx, key, kind)
}

// Clear takes back the failures of one kind (empty kind clears them all) after
// a correct password or code.
func (s Store) Clear(ctx context.Context, key, kind string) error {
	_, err := s.DB.ExecContext(ctx,
		`DELETE FROM login_failures WHERE account_key = ? AND (? = '' OR kind = ?)`, key, kind, kind)
	return err
}
