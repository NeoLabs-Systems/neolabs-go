package sessionx

import (
	"context"
	"time"

	"github.com/NeoLabs-Systems/neolabs-go/passhash"
)

// Bearer sessions are for apps whose clients send a token instead of holding a
// cookie. They live in the same store, so listing and revoking an account's
// sessions work the same way. Only the token's digest is stored: a leaked
// database holds no usable token.

// IssueBearer stores data under a new token starting with prefix. data should
// carry "userId" so DestroyForUser can find it.
func (s *Store) IssueBearer(ctx context.Context, prefix string, data map[string]any, maxAge time.Duration) (token string, expires time.Time, err error) {
	random, err := passhash.RandomToken(32)
	if err != nil {
		return "", time.Time{}, err
	}
	token = prefix + random
	expires = time.Now().Add(maxAge)
	if err := s.Set(ctx, passhash.DigestToken(token), data, maxAge); err != nil {
		return "", time.Time{}, err
	}
	return token, expires, nil
}

// Bearer returns the live session a token names, and its id for Destroy. The
// data is nil when the token is unknown, expired or revoked.
func (s *Store) Bearer(ctx context.Context, token string) (sid string, data map[string]any, err error) {
	sid = passhash.DigestToken(token)
	data, err = s.Get(ctx, sid)
	return sid, data, err
}
