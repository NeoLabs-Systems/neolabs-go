package account

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/mail"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/NeoLabs-Systems/neolabs-go/passhash"
	"github.com/NeoLabs-Systems/neolabs-go/servicemail"
	"github.com/NeoLabs-Systems/neolabs-go/userid"
)

// Token types stored in user_email_tokens.type.
const (
	TokenPasswordReset = "password_reset"
	TokenSignup        = "signup_confirmation"
	TokenEmailChange   = "email_change_confirmation"
)

// mailWindow is the span MailCap counts over.
const mailWindow = 15 * time.Minute

// Messages word the account emails in the app's voice. Each gets the link the
// email must carry.
type Messages struct {
	Reset  func(a Account, link string) servicemail.Message
	Signup func(a Account, link string) servicemail.Message
	// Change goes to the new address and confirms it.
	Change func(a Account, newEmail, link string) servicemail.Message
	// ChangeRequested, when set, tells the current address that a change was
	// asked for.
	ChangeRequested func(a Account, newEmail string) servicemail.Message
}

// EventKind names what a flow changed.
type EventKind int

// The changes reported to Recovery.Changed.
const (
	PasswordReset  EventKind = iota + 1
	EmailConfirmed           // the sign-up address was confirmed
	EmailChanged             // the address was replaced
)

// Event is one committed change, for notices and audit entries.
type Event struct {
	Kind     EventKind
	Account  Account // as it was before the change
	Email    string  // the reset, confirmed or new address
	Verified bool    // the address was proven by a link
}

// Revoker ends every session of an account; *sessionx.Store satisfies it.
type Revoker interface {
	DestroyForUser(ctx context.Context, userID string) error
}

// Querier reads one row; *sql.DB and *sql.Tx satisfy it.
type Querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Recovery runs the emailed-link flows: password reset, sign-up confirmation
// and email change. Tokens live in user_email_tokens (see servicemail.Tokens).
type Recovery struct {
	DB        *sql.DB // writer holding users and user_email_tokens
	Directory Directory
	Mailer    servicemail.Mailer // nil while service email is off
	// PublicURL is the configured address links are built from (servicemail
	// Config.PublicURL); never the request's Host.
	PublicURL   string
	ResetPath   string // e.g. "/api/auth/password/reset"
	ConfirmPath string // e.g. "/api/auth/email/confirm"
	TTL         time.Duration
	MailCap     int // tokens of one type per account per 15 minutes; 3 when 0
	// RequireChangeConfirmation sends a link to a new address instead of
	// setting it at once.
	RequireChangeConfirmation bool
	Messages                  Messages
	// MessagesFor, when set, words the emails for a request's context (its
	// language, say) instead of Messages. Mail sent in the background keeps
	// the request's context values.
	MessagesFor func(ctx context.Context) Messages

	// CanReset narrows who may get a reset link, beyond accounts that are
	// enabled and have an address.
	CanReset         func(a Account) bool
	ValidatePassword func(password string) error
	// SetPassword hashes and stores the new password inside the transaction
	// that spends the link; revoke app credentials kept in the same database
	// here too.
	SetPassword func(ctx context.Context, tx *sql.Tx, id userid.ID, password string) error
	// SetEmail stores an address; verified tells whether a link proved it.
	// The default updates users.email and users.email_verified_at.
	SetEmail func(ctx context.Context, tx *sql.Tx, id userid.ID, email string, verified bool) error
	// EmailInUse reports another account with the address; the default reads
	// users.email.
	EmailInUse func(ctx context.Context, q Querier, email string, except userid.ID) (bool, error)
	// Sessions are ended after a password reset.
	Sessions Revoker
	// Changed runs after a change committed.
	Changed  func(ctx context.Context, e Event)
	Attempts *servicemail.AttemptLimiter
	Log      *slog.Logger

	wg              sync.WaitGroup
	defaultAttempts servicemail.AttemptLimiter
}

func (r *Recovery) tokens() servicemail.Tokens { return servicemail.Tokens{DB: r.DB} }

func (r *Recovery) messages(ctx context.Context) Messages {
	if r.MessagesFor != nil {
		return r.MessagesFor(ctx)
	}
	return r.Messages
}

func (r *Recovery) log() *slog.Logger {
	if r.Log != nil {
		return r.Log
	}
	return slog.Default()
}

// Enabled reports whether links can be emailed: a mailer and a usable public
// URL are configured.
func (r *Recovery) Enabled() bool {
	_, err := r.link("/", "x")
	return r.Mailer != nil && err == nil
}

func (r *Recovery) link(path, token string) (string, error) {
	u, err := url.Parse(r.PublicURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return "", errors.New("account: the public URL for email links is not configured")
	}
	return strings.TrimRight(r.PublicURL, "/") + path + "?token=" + url.QueryEscape(token), nil
}

// Go runs mail work in the background, bounded to 30 seconds and tracked by
// Wait, so an anonymous request answers before SMTP does.
func (r *Recovery) Go(work func(ctx context.Context) error) { r.goFrom(context.Background(), work) }

// goFrom is Go keeping parent's values but not its cancellation.
func (r *Recovery) goFrom(parent context.Context, work func(ctx context.Context) error) {
	r.wg.Go(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), 30*time.Second)
		defer cancel()
		if err := work(ctx); err != nil {
			r.log().Warn("account email", "err", err)
		}
	})
}

// Wait blocks until background mail work finished.
func (r *Recovery) Wait() { r.wg.Wait() }

// send issues a token of typ for the account and mails its link to an
// address, within the per-account cap. A failed send retires the token.
func (r *Recovery) send(ctx context.Context, a Account, typ, to string, build func(m Messages, link string) servicemail.Message) error {
	if !r.Enabled() {
		return ErrEmailOff
	}
	limit := r.MailCap
	if limit <= 0 {
		limit = 3
	}
	tk := r.tokens()
	n, err := tk.Recent(ctx, a.ID, typ, time.Now().Add(-mailWindow))
	if err != nil {
		return err
	}
	if n >= limit {
		return ErrMailLimited
	}
	ttl := r.TTL
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	path := r.ConfirmPath
	if typ == TokenPasswordReset {
		path = r.ResetPath
	}
	token, _, err := tk.Create(ctx, a.ID, typ, to, ttl)
	if err != nil {
		return err
	}
	link, err := r.link(path, token)
	if err != nil {
		return err
	}
	if err := r.Mailer.Send(ctx, to, build(r.messages(ctx), link)); err != nil {
		_ = tk.Retire(context.WithoutCancel(ctx), r.DB, a.ID, typ)
		if errors.Is(err, servicemail.ErrNotConfigured) {
			return ErrEmailOff
		}
		r.log().Warn("account email not sent", "type", typ, "err", err)
		return ErrSendFailed
	}
	return nil
}

// bind gives send the wording of one email, or nil when the app words none,
// which send answers with ErrEmailOff.
func bind(word func(a Account, link string) servicemail.Message, a Account) func(link string) servicemail.Message {
	if word == nil {
		return nil
	}
	return func(link string) servicemail.Message { return word(a, link) }
}

func (r *Recovery) changeMessage(a Account, email string) func(link string) servicemail.Message {
	if r.Messages.Change == nil {
		return nil
	}
	return func(link string) servicemail.Message { return r.Messages.Change(a, email, link) }
}

// RequestReset mails a reset link when the login names an enabled account
// with an address. It reports nothing about the account to its caller beyond
// errors meant for the log.
func (r *Recovery) RequestReset(ctx context.Context, login string) error {
	if !r.Enabled() || login == "" {
		return nil
	}
	a, err := r.Directory.Lookup(ctx, login)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if a.Disabled || a.Email == "" || (r.CanReset != nil && !r.CanReset(a)) {
		return nil
	}
	err = r.send(ctx, a, TokenPasswordReset, a.Email, bind(r.Messages.Reset, a))
	if errors.Is(err, ErrMailLimited) {
		return nil
	}
	return err
}

// sameAddress reports whether the token was sent to the account's current
// address; a link mailed to an address the account no longer has is dead.
func sameAddress(t *servicemail.Row) bool {
	return t.CurrentEmail.Valid && strings.EqualFold(strings.TrimSpace(t.CurrentEmail.String), strings.TrimSpace(t.Email))
}

func (r *Recovery) attempts() *servicemail.AttemptLimiter {
	if r.Attempts != nil {
		return r.Attempts
	}
	return &r.defaultAttempts
}

// Reset sets a new password with a reset link. confirm, when given, must
// match. The password is checked against the policy before the link is spent,
// so a refused password can be corrected with the same link.
func (r *Recovery) Reset(ctx context.Context, token, password, confirm string) error {
	if token == "" {
		return ErrResetLink
	}
	if len(password) > maxSecret || len(confirm) > maxSecret {
		return ErrPasswordTooLong
	}
	key := passhash.DigestToken(token)
	lim := r.attempts()
	if lim.Allowed(key) != nil {
		return ErrTooManyTries
	}
	if confirm != "" && confirm != password {
		return ErrPasswordMismatch
	}
	tk := r.tokens()
	row, err := tk.Find(ctx, r.DB, token, TokenPasswordReset)
	if err != nil {
		return err
	}
	var a Account
	if row != nil && sameAddress(row) {
		a, err = r.Directory.Account(ctx, row.UserID)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
	}
	if a.ID == "" || a.Disabled {
		lim.Register(key)
		return ErrResetLink
	}
	if r.ValidatePassword != nil {
		if err := r.ValidatePassword(password); err != nil {
			return weakPassword(err)
		}
	}
	err = r.inTx(ctx, func(tx *sql.Tx) error {
		if err := tk.Consume(ctx, tx, row.ID); err != nil {
			return err
		}
		if err := r.SetPassword(ctx, tx, a.ID, password); err != nil {
			return err
		}
		return tk.Retire(ctx, tx, a.ID, TokenPasswordReset)
	})
	if errors.Is(err, servicemail.ErrTokenSpent) {
		return ErrResetLink
	}
	if err != nil {
		return err
	}
	lim.Reset(key)
	after := context.WithoutCancel(ctx)
	if r.Sessions != nil {
		if err := r.Sessions.DestroyForUser(after, string(a.ID)); err != nil {
			r.log().Error("account: sessions not revoked after password reset", "err", err)
		}
	}
	r.changed(after, Event{Kind: PasswordReset, Account: a, Email: row.Email, Verified: true})
	return nil
}

func weakPassword(err error) error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return &Error{Status: 400, Code: "WEAK_PASSWORD", Message: err.Error()}
}

func (r *Recovery) changed(ctx context.Context, e Event) {
	if r.Changed != nil {
		r.Changed(ctx, e)
	}
}

func (r *Recovery) inTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := r.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

func (r *Recovery) setEmail(ctx context.Context, tx *sql.Tx, id userid.ID, email string, verified bool) error {
	if r.SetEmail != nil {
		return r.SetEmail(ctx, tx, id, email, verified)
	}
	_, err := tx.ExecContext(ctx, `UPDATE users SET
		email_verified_at = CASE WHEN ?2 = 0 THEN NULL
			WHEN lower(email) = lower(?1) THEN COALESCE(email_verified_at, datetime('now'))
			ELSE datetime('now') END,
		email = ?1
		WHERE id = ?3`, email, verified, idArg(id))
	return err
}

func (r *Recovery) emailInUse(ctx context.Context, q Querier, email string, except userid.ID) (bool, error) {
	if r.EmailInUse != nil {
		return r.EmailInUse(ctx, q, email, except)
	}
	var one int
	err := q.QueryRowContext(ctx, `SELECT 1 FROM users WHERE lower(email) = lower(?) AND id <> ? LIMIT 1`, email, idArg(except)).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// ConfirmEmail spends a sign-up or email-change link and stores the address
// it proves.
func (r *Recovery) ConfirmEmail(ctx context.Context, token string) (Event, error) {
	if token == "" {
		return Event{}, ErrConfirmLink
	}
	key := passhash.DigestToken(token)
	lim := r.attempts()
	if lim.Allowed(key) != nil {
		return Event{}, ErrTooManyTries
	}
	tk := r.tokens()
	row, err := tk.Find(ctx, r.DB, token, "")
	if err != nil {
		return Event{}, err
	}
	var a Account
	if row != nil && (row.Type == TokenEmailChange || (row.Type == TokenSignup && sameAddress(row))) {
		a, err = r.Directory.Account(ctx, row.UserID)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return Event{}, err
		}
	}
	if a.ID == "" || a.Disabled {
		lim.Register(key)
		return Event{}, ErrConfirmLink
	}
	change := row.Type == TokenEmailChange
	err = r.inTx(ctx, func(tx *sql.Tx) error {
		if change {
			taken, err := r.emailInUse(ctx, tx, row.Email, a.ID)
			if err != nil {
				return err
			}
			if taken {
				return ErrEmailTaken
			}
		}
		if err := tk.Consume(ctx, tx, row.ID); err != nil {
			return err
		}
		if err := r.setEmail(ctx, tx, a.ID, row.Email, true); err != nil {
			return err
		}
		if change {
			// Links mailed to the old address must not outlive it.
			return tk.Retire(ctx, tx, a.ID, TokenSignup, TokenEmailChange, TokenPasswordReset)
		}
		return tk.Retire(ctx, tx, a.ID, TokenSignup)
	})
	if errors.Is(err, servicemail.ErrTokenSpent) {
		return Event{}, ErrConfirmLink
	}
	if err != nil {
		return Event{}, err
	}
	lim.Reset(key)
	ev := Event{Kind: EmailConfirmed, Account: a, Email: row.Email, Verified: true}
	if change {
		ev.Kind = EmailChanged
	}
	r.changed(context.WithoutCancel(ctx), ev)
	return ev, nil
}

// SendSignupConfirmation mails the link that confirms the account's address,
// at sign-up or when asked again.
func (r *Recovery) SendSignupConfirmation(ctx context.Context, a Account) error {
	if a.Email == "" || a.EmailVerified {
		return ErrNothingToConfirm
	}
	return r.send(ctx, a, TokenSignup, a.Email, bind(r.Messages.Signup, a))
}

// ResendSignupConfirmation is the anonymous "send the link again": it mails
// only an enabled account whose address is unconfirmed and reports nothing
// about the account.
func (r *Recovery) ResendSignupConfirmation(ctx context.Context, login string) error {
	if !r.Enabled() || login == "" {
		return nil
	}
	a, err := r.Directory.Lookup(ctx, login)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if a.Disabled || a.Email == "" || a.EmailVerified {
		return nil
	}
	err = r.SendSignupConfirmation(ctx, a)
	if errors.Is(err, ErrMailLimited) {
		return nil
	}
	return err
}

// PendingEmail is the address waiting for confirmation, or "".
func (r *Recovery) PendingEmail(ctx context.Context, id userid.ID) (string, error) {
	return r.tokens().Pending(ctx, id, TokenEmailChange)
}

// NormalizeEmail trims and lower-cases an address and checks that it is one
// plain address.
func NormalizeEmail(email string) (string, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	addr, err := mail.ParseAddress(email)
	if err != nil || addr.Address != email || len(email) > 320 {
		return "", ErrInvalidEmail
	}
	return email, nil
}

// ChangeEmail replaces the signed-in account's address. With confirmation
// required it mails a link to the new address and returns it as pending;
// otherwise it stores the address, unverified, at once. Re-authenticate
// before calling it.
func (r *Recovery) ChangeEmail(ctx context.Context, a Account, email string) (pending string, err error) {
	email, err = NormalizeEmail(email)
	if err != nil {
		return "", err
	}
	tk := r.tokens()
	if strings.EqualFold(email, a.Email) {
		return "", tk.Retire(ctx, r.DB, a.ID, TokenEmailChange)
	}
	if r.RequireChangeConfirmation {
		if !r.Enabled() {
			return "", ErrEmailOff
		}
		taken, err := r.emailInUse(ctx, r.DB, email, a.ID)
		if err != nil {
			return "", err
		}
		if taken {
			return "", ErrEmailTaken
		}
		if err := r.send(ctx, a, TokenEmailChange, email, r.changeMessage(a, email)); err != nil {
			return "", err
		}
		if notice := r.messages(ctx).ChangeRequested; notice != nil && a.Email != "" {
			r.goFrom(ctx, func(ctx context.Context) error { return r.Mailer.Send(ctx, a.Email, notice(a, email)) })
		}
		return email, nil
	}
	err = r.inTx(ctx, func(tx *sql.Tx) error {
		taken, err := r.emailInUse(ctx, tx, email, a.ID)
		if err != nil {
			return err
		}
		if taken {
			return ErrEmailTaken
		}
		if err := r.setEmail(ctx, tx, a.ID, email, false); err != nil {
			return err
		}
		return tk.Retire(ctx, tx, a.ID)
	})
	if err != nil {
		return "", err
	}
	r.changed(context.WithoutCancel(ctx), Event{Kind: EmailChanged, Account: a, Email: email})
	return "", nil
}

// ResendConfirmation mails the pending change link again, or the sign-up
// link while the address is unconfirmed. It returns the pending address.
func (r *Recovery) ResendConfirmation(ctx context.Context, a Account) (pending string, err error) {
	pending, err = r.PendingEmail(ctx, a.ID)
	if err != nil {
		return "", err
	}
	if pending == "" {
		return "", r.SendSignupConfirmation(ctx, a)
	}
	return pending, r.send(ctx, a, TokenEmailChange, pending, r.changeMessage(a, pending))
}
