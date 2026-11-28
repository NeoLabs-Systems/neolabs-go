package account

import (
	"context"

	"github.com/NeoLabs-Systems/neolabs-go/lockout"
	"github.com/NeoLabs-Systems/neolabs-go/userid"
)

// Factors are the secrets an account can confirm itself with.
type Factors struct {
	Password  bool // the account signs in with a password
	TwoFactor bool // 2FA (TOTP or recovery codes) is on
}

// Verifier checks an account's secrets. CheckCode accepts a TOTP code or an
// unused recovery code and spends what it accepts (replay guard, used_at).
type Verifier interface {
	Factors(ctx context.Context, id userid.ID) (Factors, error)
	CheckPassword(ctx context.Context, id userid.ID, password string) (bool, error)
	CheckCode(ctx context.Context, id userid.ID, code string) (bool, error)
}

// Reauth confirms that the person at a signed-in session knows the account's
// secrets before a sensitive action. Every try counts against the account's
// confirmation lockout before the secret is checked, checked and counted as
// one step (lockout.Store.Attempt); a success takes its failures back.
type Reauth struct {
	Lockout  lockout.Store
	Verifier Verifier
}

// Confirm checks the password (when the account has one) and the 2FA code
// (when 2FA is on). requireFactor refuses accounts without 2FA, for actions
// that hand out secrets. An account with neither factor is confirmed by its
// session alone.
func (a Reauth) Confirm(ctx context.Context, id userid.ID, password, code string, requireFactor bool) error {
	f, err := a.Verifier.Factors(ctx, id)
	if err != nil {
		return err
	}
	if requireFactor && !f.TwoFactor {
		return ErrFactorRequired
	}
	key := lockout.ConfirmKey(string(id))
	if f.Password || f.TwoFactor {
		if err := a.Lockout.Check(ctx, key); err != nil {
			return err
		}
	}
	if f.Password {
		if err := a.check(ctx, key, "password", ErrWrongPassword, func() (bool, error) {
			if password == "" || len(password) > maxSecret {
				return false, nil
			}
			return a.Verifier.CheckPassword(ctx, id, password)
		}); err != nil {
			return err
		}
	}
	if f.TwoFactor {
		if code == "" {
			return ErrCodeRequired
		}
		return a.check(ctx, key, "second_factor", ErrWrongCode, func() (bool, error) {
			if len(code) > maxSecret {
				return false, nil
			}
			return a.Verifier.CheckCode(ctx, id, code)
		})
	}
	return nil
}

// maxSecret bounds what is handed to bcrypt or the code check.
const maxSecret = 1024

func (a Reauth) check(ctx context.Context, key, kind string, wrong *Error, verify func() (bool, error)) error {
	if err := a.Lockout.Attempt(ctx, key, kind); err != nil {
		return err
	}
	ok, err := verify()
	if err != nil {
		return err
	}
	if !ok {
		return wrong
	}
	return a.Lockout.Clear(ctx, key, kind)
}
