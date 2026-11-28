package account

import (
	"net/http"

	"github.com/NeoLabs-Systems/neolabs-go/authpages"
)

// Pages render the browser pages emailed links open. Apps with their own
// localized pages implement it; AuthPages uses package authpages.
type Pages interface {
	// Reset shows the reset form, its refusal or, when done, its success.
	Reset(w http.ResponseWriter, r *http.Request, token string, err error, done bool)
	// Confirm shows the confirm button (ev and err nil), the confirmed
	// change, or the refusal.
	Confirm(w http.ResponseWriter, r *http.Request, token string, ev *Event, err error)
}

// AuthPages are the shared pages. Empty texts take English defaults.
type AuthPages struct {
	Brand       string
	ResetPath   string // where the reset form posts
	ConfirmPath string // where the confirm button posts

	ReadyTitle, ReadyMessage, ConfirmButton string
	ConfirmedTitle, ConfirmedMessage        string
	ChangedTitle, ChangedMessage            string
	FailedTitle                             string
}

func or(s, def string) string {
	if s != "" {
		return s
	}
	return def
}

// Reset implements Pages.
func (p AuthPages) Reset(w http.ResponseWriter, _ *http.Request, token string, err error, done bool) {
	problem := ""
	if err != nil {
		problem = Describe(err).Message
	}
	authpages.PasswordReset(w, p.Brand, p.ResetPath, token, problem, done)
}

// Confirm implements Pages.
func (p AuthPages) Confirm(w http.ResponseWriter, _ *http.Request, token string, ev *Event, err error) {
	switch {
	case err != nil:
		authpages.Confirmation(w, p.Brand, false, or(p.FailedTitle, "Email not confirmed"), Describe(err).Message)
	case ev == nil:
		authpages.EmailConfirm(w, p.Brand, p.ConfirmPath, token,
			or(p.ReadyTitle, "Confirm your email"),
			or(p.ReadyMessage, "Confirm this address for your "+p.Brand+" account."),
			or(p.ConfirmButton, "Confirm email"))
	case ev.Kind == EmailChanged:
		authpages.Confirmation(w, p.Brand, true, or(p.ChangedTitle, "Email changed"),
			or(p.ChangedMessage, "Your "+p.Brand+" account now uses "+ev.Email+"."))
	default:
		authpages.Confirmation(w, p.Brand, true, or(p.ConfirmedTitle, "Email confirmed"),
			or(p.ConfirmedMessage, "Your email address is confirmed. You can return to "+p.Brand+"."))
	}
}
