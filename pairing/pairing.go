// Package pairing signs a phone in with a code shown on a signed-in screen: the
// screen mints a short-lived single-use code, the phone scans it and swaps it
// for a session of that account. Only the code's SHA-256 is stored, in
// user_device_pairings.
//
// The other direction (a screen that is not signed in shows a code, a signed-in
// phone approves it) is package qrlogin.
package pairing

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/NeoLabs-Systems/neolabs-go/apperr"
	"github.com/NeoLabs-Systems/neolabs-go/dbx"
	"github.com/NeoLabs-Systems/neolabs-go/passhash"
)

const (
	lifetimeSeconds = 300
	maxOpenCodes    = 5
	maxLabelLength  = 80
)

// Store mints and redeems codes.
type Store struct {
	DB *dbx.DB
	// Scheme is the app's URL scheme, e.g. "neoagent" for neoagent://pair?...
	Scheme string
}

// Started is what the signed-in screen shows.
type Started struct {
	Code      string `json:"code"`
	ExpiresAt string `json:"expiresAt"`
	Server    string `json:"server"`
	QRPayload string `json:"qrPayload"`
}

func cleanLabel(v string) string {
	v = strings.Join(strings.Fields(v), " ")
	if r := []rune(v); len(r) > maxLabelLength {
		v = string(r[:maxLabelLength])
	}
	return v
}

// Start mints a code. serverURL is where the phone reaches this server, base
// path included.
func (s *Store) Start(ctx context.Context, userID int64, serverURL string) (*Started, error) {
	// Housekeeping must never fail a request.
	_, _ = s.DB.W.ExecContext(ctx, `DELETE FROM user_device_pairings WHERE datetime(expires_at) < datetime('now', '-1 day')`)
	var open int
	if err := s.DB.W.QueryRowContext(ctx, `SELECT COUNT(*) FROM user_device_pairings
		WHERE user_id = ? AND status = 'pending' AND datetime(expires_at) > datetime('now')`, userID).Scan(&open); err != nil {
		return nil, err
	}
	if open >= maxOpenCodes {
		return nil, apperr.New(429, "Too many open pairing codes. Wait a few minutes and try again.", "TOO_MANY_PAIRING_CODES")
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, fmt.Errorf("pairing: random code: %w", err)
	}
	code := base64.RawURLEncoding.EncodeToString(raw)
	var expires string
	err := s.DB.W.QueryRowContext(ctx, `INSERT INTO user_device_pairings (user_id, code_hash, expires_at)
		VALUES (?, ?, datetime('now', ?)) RETURNING expires_at`,
		userID, passhash.DigestToken(code), fmt.Sprintf("+%d seconds", lifetimeSeconds)).Scan(&expires)
	if err != nil {
		return nil, err
	}
	payload := fmt.Sprintf("%s://pair?server=%s&code=%s", s.Scheme, url.QueryEscape(serverURL), url.QueryEscape(code))
	return &Started{Code: code, ExpiresAt: expires, Server: serverURL, QRPayload: payload}, nil
}

// Device is the phone that redeemed a code.
type Device struct {
	Name string `json:"name"`
}

// Status is the pairing dialog's poll answer.
type Status struct {
	Status string  `json:"status"`
	Device *Device `json:"device,omitempty"`
}

// MarshalJSON keeps `device` (null) on a used code without a name, like Node.
func (st Status) MarshalJSON() ([]byte, error) {
	if st.Status == "used" {
		if st.Device == nil {
			return []byte(`{"status":"used","device":null}`), nil
		}
		return []byte(fmt.Sprintf(`{"status":"used","device":{"name":%q}}`, st.Device.Name)), nil
	}
	return []byte(fmt.Sprintf(`{"status":%q}`, st.Status)), nil
}

func (s *Store) expired(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, expiresAt string) (bool, error) {
	var e int
	err := q.QueryRowContext(ctx, `SELECT datetime(?) <= datetime('now')`, expiresAt).Scan(&e)
	return e == 1, err
}

// Status reports whether the account's code is pending, used or expired.
func (s *Store) Status(ctx context.Context, userID int64, code string) (*Status, error) {
	var status, expiresAt string
	var label sql.NullString
	err := s.DB.W.QueryRowContext(ctx, `SELECT status, expires_at, device_label FROM user_device_pairings WHERE code_hash = ? AND user_id = ?`,
		passhash.DigestToken(code), userID).Scan(&status, &expiresAt, &label)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, apperr.New(404, "Pairing code not found", "NOT_FOUND")
	}
	if err != nil {
		return nil, err
	}
	if status == "used" {
		st := &Status{Status: "used"}
		if label.Valid && label.String != "" {
			st.Device = &Device{Name: label.String}
		}
		return st, nil
	}
	exp, err := s.expired(ctx, s.DB.W, expiresAt)
	if err != nil {
		return nil, err
	}
	if exp {
		return &Status{Status: "expired"}, nil
	}
	return &Status{Status: "pending"}, nil
}

// Redeem is the phone's swap: it spends the code and returns the account to sign
// in as. Used, expired and unknown codes all answer 410.
func (s *Store) Redeem(ctx context.Context, code, deviceLabel string) (userID int64, err error) {
	gone := apperr.New(410, "The pairing code has expired. Show a new one on the signed-in screen.", "EXPIRED_TOKEN")
	if code == "" {
		return 0, gone
	}
	tx, err := s.DB.W.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var id int64
	var status, expiresAt string
	err = tx.QueryRowContext(ctx, `SELECT id, user_id, status, expires_at FROM user_device_pairings WHERE code_hash = ?`,
		passhash.DigestToken(code)).Scan(&id, &userID, &status, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, gone
	}
	if err != nil {
		return 0, err
	}
	exp, err := s.expired(ctx, tx, expiresAt)
	if err != nil {
		return 0, err
	}
	if status != "pending" || exp {
		return 0, gone
	}
	var one int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM users WHERE id = ?`, userID).Scan(&one); errors.Is(err, sql.ErrNoRows) {
		return 0, apperr.New(410, "The account that showed this code is no longer available.", "EXPIRED_TOKEN")
	} else if err != nil {
		return 0, err
	}
	var label any
	if l := cleanLabel(deviceLabel); l != "" {
		label = l
	}
	res, err := tx.ExecContext(ctx, `UPDATE user_device_pairings SET status = 'used', device_label = ?, used_at = datetime('now')
		WHERE id = ? AND status = 'pending'`, label, id)
	if err != nil {
		return 0, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return 0, gone
	}
	return userID, tx.Commit()
}
