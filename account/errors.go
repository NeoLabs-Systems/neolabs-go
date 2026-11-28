package account

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/NeoLabs-Systems/neolabs-go/httpx"
	"github.com/NeoLabs-Systems/neolabs-go/lockout"
)

// Error is a refusal clients can act on. Apps that use other codes or another
// error envelope match the sentinels below with errors.Is in their own
// WriteError.
type Error struct {
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Message }

// The refusals of the account flows.
var (
	ErrUnauthorized     = &Error{http.StatusUnauthorized, "UNAUTHORIZED", "Sign in to continue."}
	ErrBadRequest       = &Error{http.StatusBadRequest, "INVALID_REQUEST", "The request is not valid."}
	ErrNotFound         = &Error{http.StatusNotFound, "NOT_FOUND", "Account not found."}
	ErrWrongPassword    = &Error{http.StatusUnauthorized, "WRONG_PASSWORD", "Current password is incorrect."}
	ErrCodeRequired     = &Error{http.StatusUnauthorized, "TWO_FACTOR_REQUIRED", "A two-factor code is required."}
	ErrWrongCode        = &Error{http.StatusUnauthorized, "INVALID_CODE", "The two-factor code is not valid."}
	ErrFactorRequired   = &Error{http.StatusForbidden, "TWO_FACTOR_SETUP_REQUIRED", "Turn on two-factor authentication first."}
	ErrConfirmMismatch  = &Error{http.StatusBadRequest, "CONFIRMATION_MISMATCH", "Type your username to confirm."}
	ErrLastAdmin        = &Error{http.StatusConflict, "LAST_ADMIN", "You are the only admin. Make someone else an admin first."}
	ErrAdminSelfDelete  = &Error{http.StatusForbidden, "ADMIN_SELF_DELETE", "Admin accounts can't delete themselves. Remove the admin role first."}
	ErrAdminAccount     = &Error{http.StatusForbidden, "ADMIN_ACCOUNT", "Remove admin rights before deleting this account."}
	ErrDeleteSelf       = &Error{http.StatusBadRequest, "CANNOT_DELETE_SELF", "Delete your own account from your account settings."}
	ErrResetLink        = &Error{http.StatusBadRequest, "INVALID_RESET_LINK", "Password reset link is invalid or expired."}
	ErrConfirmLink      = &Error{http.StatusBadRequest, "INVALID_CONFIRMATION_LINK", "Confirmation link is invalid or expired."}
	ErrTooManyTries     = &Error{http.StatusTooManyRequests, "RATE_LIMITED", "Too many attempts with this link. Try again later."}
	ErrPasswordMismatch = &Error{http.StatusBadRequest, "PASSWORD_MISMATCH", "Passwords do not match."}
	ErrPasswordTooLong  = &Error{http.StatusBadRequest, "PASSWORD_TOO_LONG", "The password is too long."}
	ErrInvalidEmail     = &Error{http.StatusBadRequest, "INVALID_EMAIL", "A valid email address is required."}
	ErrEmailTaken       = &Error{http.StatusConflict, "EMAIL_TAKEN", "Email is already in use."}
	ErrEmailOff         = &Error{http.StatusConflict, "EMAIL_NOT_CONFIGURED", "Service email is not configured."}
	ErrMailLimited      = &Error{http.StatusTooManyRequests, "RATE_LIMITED", "Too many emails for this account. Try again later."}
	ErrSendFailed       = &Error{http.StatusServiceUnavailable, "EMAIL_SEND_FAILED", "The email could not be sent. Try again later."}
	ErrNothingToConfirm = &Error{http.StatusConflict, "NOTHING_TO_CONFIRM", "There is no address waiting for confirmation."}
	ErrSessionNotFound  = &Error{http.StatusNotFound, "NOT_FOUND", "Session not found."}
	ErrCurrentSession   = &Error{http.StatusBadRequest, "CURRENT_SESSION", "Use sign out to end the current session."}
)

// errInternal answers failures that are not the client's to act on.
var errInternal = &Error{http.StatusInternalServerError, "INTERNAL_ERROR", "Something went wrong. Please try again."}

// Problem is a refusal in plain fields, for apps writing their own envelope.
type Problem struct {
	Status     int
	Code       string
	Message    string
	RetryAfter int // seconds; set for throttled confirmations
}

// Describe turns any error from this package into what the client is told.
// Errors it does not know become a 500 that reveals nothing.
func Describe(err error) Problem {
	var locked *lockout.LockedError
	if errors.As(err, &locked) {
		return Problem{Status: locked.Status(), Code: locked.Code(), Message: locked.Error(), RetryAfter: locked.RetryAfter}
	}
	var e *Error
	if !errors.As(err, &e) {
		e = errInternal
	}
	return Problem{Status: e.Status, Code: e.Code, Message: e.Message}
}

// WriteError answers {"error": message, "code": code} (plus "retryAfter" and a
// Retry-After header when throttled), the envelope NeoAgent and NeoCloud use.
func WriteError(w http.ResponseWriter, _ *http.Request, err error) {
	p := Describe(err)
	body := map[string]any{"error": p.Message, "code": p.Code}
	if p.RetryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(p.RetryAfter))
		body["retryAfter"] = p.RetryAfter
	}
	httpx.WriteJSON(w, p.Status, body)
}
