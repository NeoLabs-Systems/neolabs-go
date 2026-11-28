package account

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"time"

	"github.com/NeoLabs-Systems/neolabs-go/sessionx"
	"github.com/NeoLabs-Systems/neolabs-go/userid"
)

// Session is one signed-in device of an account as session lists show it.
type Session struct {
	ID         string // numeric ids are written as JSON numbers
	Current    bool
	IPAddress  string
	UserAgent  string
	CreatedAt  string
	LastSeenAt string
	ExpiresAt  string
	Extra      map[string]any // app fields such as "location"
}

// MarshalJSON writes {"id","current","ipAddress","userAgent","createdAt",
// "lastSeenAt","expiresAt"} plus Extra; empty texts are null.
func (s Session) MarshalJSON() ([]byte, error) {
	m := make(map[string]any, 7+len(s.Extra))
	for k, v := range s.Extra {
		m[k] = v
	}
	text := func(v string) any {
		if v == "" {
			return nil
		}
		return v
	}
	m["id"] = s.ID
	if n, err := strconv.ParseInt(s.ID, 10, 64); err == nil && strconv.FormatInt(n, 10) == s.ID {
		m["id"] = n
	}
	m["current"] = s.Current
	m["ipAddress"], m["userAgent"] = text(s.IPAddress), text(s.UserAgent)
	m["createdAt"], m["lastSeenAt"], m["expiresAt"] = text(s.CreatedAt), text(s.LastSeenAt), text(s.ExpiresAt)
	return json.Marshal(m)
}

// Sessions lists and ends an account's sessions. current is the id the
// request's own session has in the store (CurrentSession).
type Sessions interface {
	List(ctx context.Context, user userid.ID, current string) ([]Session, error)
	// Revoke ends one other session; ErrSessionNotFound for an unknown id,
	// ErrCurrentSession for the caller's own.
	Revoke(ctx context.Context, user userid.ID, id, current string) error
	// RevokeAll ends every session but keep ("" ends all).
	RevokeAll(ctx context.Context, user userid.ID, keep string) error
}

// StoreSessions lists sessions straight from a sessionx store, for apps
// without a session record table. Session data may carry "ipAddress",
// "userAgent", "createdAt" and "lastSeenAt". Store ids never leave the
// server: a session is named by a digest of its id. current is the store id:
// the cookie session's ID(), or for bearer tokens the sid Bearer returns.
type StoreSessions struct{ Store *sessionx.Store }

// PublicSessionID is the name a stored session has in session lists.
func PublicSessionID(sid string) string {
	sum := sha256.Sum256([]byte("neolabs-session:" + sid))
	return hex.EncodeToString(sum[:12])
}

// List implements Sessions.
func (s StoreSessions) List(ctx context.Context, user userid.ID, current string) ([]Session, error) {
	rows, err := s.Store.ForUser(ctx, string(user))
	if err != nil {
		return nil, err
	}
	out := make([]Session, 0, len(rows))
	for _, r := range rows {
		str := func(k string) string { v, _ := r.Data[k].(string); return v }
		out = append(out, Session{
			ID: PublicSessionID(r.SID), Current: r.SID == current,
			IPAddress: str("ipAddress"), UserAgent: str("userAgent"),
			CreatedAt: str("createdAt"), LastSeenAt: str("lastSeenAt"),
			ExpiresAt: r.Expire.UTC().Format(time.RFC3339),
		})
	}
	return out, nil
}

// Revoke implements Sessions.
func (s StoreSessions) Revoke(ctx context.Context, user userid.ID, id, current string) error {
	if current != "" && id == PublicSessionID(current) {
		return ErrCurrentSession
	}
	rows, err := s.Store.ForUser(ctx, string(user))
	if err != nil {
		return err
	}
	for _, r := range rows {
		if PublicSessionID(r.SID) == id {
			return s.Store.Destroy(ctx, r.SID)
		}
	}
	return ErrSessionNotFound
}

// RevokeAll implements Sessions.
func (s StoreSessions) RevokeAll(ctx context.Context, user userid.ID, keep string) error {
	if keep == "" {
		return s.Store.DestroyForUser(ctx, string(user))
	}
	rows, err := s.Store.ForUser(ctx, string(user))
	if err != nil {
		return err
	}
	for _, r := range rows {
		if r.SID != keep {
			if err := s.Store.Destroy(ctx, r.SID); err != nil {
				return err
			}
		}
	}
	return nil
}
