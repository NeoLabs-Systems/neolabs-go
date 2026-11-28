// Package account is the self-service account lifecycle the NeoLabs apps
// share: password reset, email confirmation and change, re-authentication,
// data export, account deletion and session lists. The flows, their security
// rules and HTTP handlers live here; each app keeps its data behind small
// hooks (which rows to export or erase, how a password is stored, which
// sessions exist) and keeps its routes, response shapes and error codes by
// mounting the handlers on its own paths and writing errors its own way.
//
// Rules every flow keeps:
//   - emailed links are built only from the configured public URL, never from
//     the request; tokens are 256-bit, stored as SHA-256, single use, expiring
//     and spent by one conditional write in the transaction that applies them;
//     opening a link (GET) spends nothing;
//   - anonymous endpoints answer the same way, after the same minimum time,
//     whether or not the account exists;
//   - sensitive actions re-authenticate with the password and, when it is on,
//     the 2FA code, throttled per account by package lockout;
//   - deletion erases rows in one transaction, removes files and revokes
//     sessions only after it committed, and never removes the last admin.
package account

import (
	"context"
	"strconv"

	"github.com/NeoLabs-Systems/neolabs-go/userid"
)

// Account is what the flows need to know about an account.
type Account struct {
	ID            userid.ID
	Username      string
	Email         string
	EmailVerified bool
	Admin         bool // an active admin
	Disabled      bool
}

// Directory finds accounts. Both methods return ErrNotFound for an unknown
// account.
type Directory interface {
	Account(ctx context.Context, id userid.ID) (Account, error)
	// Lookup finds the account a sign-in name or email address names, as the
	// app's sign-in does.
	Lookup(ctx context.Context, login string) (Account, error)
}

// idArg binds an account id the way its column stores it: numbered accounts
// as integers, others as text.
func idArg(id userid.ID) any {
	if n, err := id.Int64(); err == nil && strconv.FormatInt(n, 10) == string(id) {
		return n
	}
	return string(id)
}
