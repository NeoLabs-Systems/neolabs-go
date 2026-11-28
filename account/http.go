package account

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/NeoLabs-Systems/neolabs-go/httpx"
	"github.com/NeoLabs-Systems/neolabs-go/userid"
)

// Fields name the JSON (or form) fields the account routes read.
type Fields struct {
	Password string // current password; "currentPassword" when empty
	Code     string // 2FA or recovery code; "code" when empty
	Confirm  string // typed confirmation; "confirmUsername" when empty
	Email    string // new address; "email" when empty
}

func (f Fields) name(v, def string) string { return or(v, def) }

// Handlers are the HTTP side of the flows. Apps mount each handler on their
// existing path behind their own auth and rate-limit middleware, and keep
// their envelope through WriteError and the Done callbacks.
type Handlers struct {
	Directory Directory
	// Identify returns the signed-in account of a request.
	Identify func(r *http.Request) (userid.ID, bool)
	// CurrentSession returns the store id of the request's session (see
	// Sessions); "" when the request carries none.
	CurrentSession func(r *http.Request) string
	// WriteError answers a refusal; account.WriteError when nil.
	WriteError func(w http.ResponseWriter, r *http.Request, err error)
	Fields     Fields
	Log        *slog.Logger

	Reauth   *Reauth
	Recovery *Recovery
	Pages    Pages
	// MinLatency pads the anonymous mail routes so their timing tells
	// nothing; 350 ms when 0.
	MinLatency time.Duration
	// AnonymousMessage is the "message" of the identical answer the
	// anonymous mail routes give; kind is TokenPasswordReset or TokenSignup.
	AnonymousMessage func(r *http.Request, kind string) string

	Exporter *Exporter
	Deleter  *Deleter
	Sessions Sessions
}

func (h *Handlers) log() *slog.Logger {
	if h.Log != nil {
		return h.Log
	}
	return slog.Default()
}

func (h *Handlers) fail(w http.ResponseWriter, r *http.Request, err error) {
	if Describe(err).Status >= 500 {
		h.log().Error("account request failed", "method", r.Method, "path", r.URL.Path, "err", err)
	}
	if h.WriteError != nil {
		h.WriteError(w, r, err)
		return
	}
	WriteError(w, r, err)
}

// readFields reads the string fields of a JSON object or form body.
func readFields(w http.ResponseWriter, r *http.Request) (map[string]string, error) {
	out := map[string]string{}
	if r.Body == nil || r.Method == http.MethodGet || r.Method == http.MethodHead {
		return out, nil
	}
	ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if ct == "application/x-www-form-urlencoded" || ct == "multipart/form-data" {
		r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
		if err := r.ParseForm(); err != nil {
			return nil, ErrBadRequest
		}
		for k, v := range r.PostForm {
			if len(v) > 0 {
				out[k] = v[0]
			}
		}
		return out, nil
	}
	var raw map[string]any
	err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&raw)
	if errors.Is(err, io.EOF) {
		return out, nil
	}
	if err != nil {
		return nil, ErrBadRequest
	}
	for k, v := range raw {
		if s, ok := v.(string); ok {
			out[k] = s
		}
	}
	return out, nil
}

// signedIn resolves the request's account and reads its body.
func (h *Handlers) signedIn(w http.ResponseWriter, r *http.Request) (Account, map[string]string, bool) {
	id, ok := h.Identify(r)
	if !ok || id == "" {
		h.fail(w, r, ErrUnauthorized)
		return Account{}, nil, false
	}
	a, err := h.Directory.Account(r.Context(), id)
	if errors.Is(err, ErrNotFound) {
		err = ErrUnauthorized
	}
	if err == nil && a.Disabled {
		err = ErrUnauthorized
	}
	if err != nil {
		h.fail(w, r, err)
		return Account{}, nil, false
	}
	f, err := readFields(w, r)
	if err != nil {
		h.fail(w, r, err)
		return Account{}, nil, false
	}
	return a, f, true
}

// confirm re-authenticates with the body's password and code.
func (h *Handlers) confirm(ctx context.Context, a Account, f map[string]string, requireFactor bool) error {
	if h.Reauth == nil {
		return errors.New("account: re-authentication is not configured")
	}
	return h.Reauth.Confirm(ctx, a.ID, f[h.Fields.name(h.Fields.Password, "currentPassword")],
		strings.TrimSpace(f[h.Fields.name(h.Fields.Code, "code")]), requireFactor)
}

func (h *Handlers) current(r *http.Request) string {
	if h.CurrentSession == nil {
		return ""
	}
	return h.CurrentSession(r)
}

// anonymous answers the same, after the same minimum time, whatever the
// account; the mail work runs in the background.
func (h *Handlers) anonymous(kind string, work func(ctx context.Context, login string) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		f, err := readFields(w, r)
		if err != nil {
			h.fail(w, r, err)
			return
		}
		login := strings.TrimSpace(f["account"])
		if len(login) > 320 {
			h.fail(w, r, ErrBadRequest)
			return
		}
		if login != "" {
			h.Recovery.Go(func(ctx context.Context) error { return work(ctx, login) })
		}
		pad := h.MinLatency
		if pad <= 0 {
			pad = 350 * time.Millisecond
		}
		if wait := pad - time.Since(start); wait > 0 {
			t := time.NewTimer(wait)
			select {
			case <-t.C:
			case <-r.Context().Done():
				t.Stop()
				return
			}
		}
		msg := ""
		if h.AnonymousMessage != nil {
			msg = h.AnonymousMessage(r, kind)
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"success": true, "message": msg})
	}
}

// Forgot answers POST {"account"} and mails a reset link in the background
// when the account exists: {"success": true, "message": ...} either way.
func (h *Handlers) Forgot() http.HandlerFunc {
	return h.anonymous(TokenPasswordReset, h.Recovery.RequestReset)
}

// ResendSignup answers POST {"account"} and mails the sign-up confirmation
// again when the account's address is unconfirmed; the same answer either
// way.
func (h *Handlers) ResendSignup() http.HandlerFunc {
	return h.anonymous(TokenSignup, h.Recovery.ResendSignupConfirmation)
}

// ResetPage shows the reset form for GET ?token=; it spends nothing.
func (h *Handlers) ResetPage() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := r.URL.Query().Get("token")
		var err error
		if token == "" {
			err = ErrResetLink
		}
		h.Pages.Reset(w, r, token, err, false)
	}
}

// ResetSubmit takes the form (or JSON) token, password and confirmPassword
// and answers with the reset page.
func (h *Handlers) ResetSubmit() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f, err := readFields(w, r)
		if err == nil {
			err = h.Recovery.Reset(r.Context(), f["token"], f["password"], f["confirmPassword"])
		}
		if err != nil && Describe(err).Status >= 500 {
			h.log().Error("password reset failed", "err", err)
		}
		h.Pages.Reset(w, r, f["token"], err, err == nil)
	}
}

// ConfirmPage shows the confirm button for GET ?token=; it spends nothing.
func (h *Handlers) ConfirmPage() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := r.URL.Query().Get("token")
		var err error
		if token == "" {
			err = ErrConfirmLink
		}
		h.Pages.Confirm(w, r, token, nil, err)
	}
}

// ConfirmSubmit spends the posted token and answers with the result page.
func (h *Handlers) ConfirmSubmit() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f, err := readFields(w, r)
		var ev Event
		if err == nil {
			ev, err = h.Recovery.ConfirmEmail(r.Context(), f["token"])
		}
		if err != nil {
			if Describe(err).Status >= 500 {
				h.log().Error("email confirmation failed", "err", err)
			}
			h.Pages.Confirm(w, r, f["token"], nil, err)
			return
		}
		h.Pages.Confirm(w, r, f["token"], &ev, nil)
	}
}

// EmailDone writes the answer to an email change or resend; pending is the
// address waiting for confirmation, "" when none.
type EmailDone func(w http.ResponseWriter, r *http.Request, a Account, pending string)

func defaultEmailDone(w http.ResponseWriter, _ *http.Request, _ Account, pending string) {
	body := map[string]any{"success": true, "pendingEmail": nil}
	if pending != "" {
		body["pendingEmail"] = pending
	}
	httpx.WriteJSON(w, http.StatusOK, body)
}

// ChangeEmail takes {"email", password, code}, re-authenticates and changes
// the signed-in account's address (see Recovery.ChangeEmail). done writes the
// answer; {"success": true, "pendingEmail": ...} when nil. The account passed
// to done is re-read after the change.
func (h *Handlers) ChangeEmail(done EmailDone) http.HandlerFunc {
	if done == nil {
		done = defaultEmailDone
	}
	return func(w http.ResponseWriter, r *http.Request) {
		a, f, ok := h.signedIn(w, r)
		if !ok {
			return
		}
		email, err := NormalizeEmail(f[h.Fields.name(h.Fields.Email, "email")])
		if err == nil {
			err = h.confirm(r.Context(), a, f, false)
		}
		var pending string
		if err == nil {
			pending, err = h.Recovery.ChangeEmail(r.Context(), a, email)
		}
		if err == nil {
			a, err = h.Directory.Account(r.Context(), a.ID)
		}
		if err != nil {
			h.fail(w, r, err)
			return
		}
		done(w, r, a, pending)
	}
}

// ResendEmail mails the pending change (or sign-up) confirmation of the
// signed-in account again.
func (h *Handlers) ResendEmail(done EmailDone) http.HandlerFunc {
	if done == nil {
		done = defaultEmailDone
	}
	return func(w http.ResponseWriter, r *http.Request) {
		a, _, ok := h.signedIn(w, r)
		if !ok {
			return
		}
		pending, err := h.Recovery.ResendConfirmation(r.Context(), a)
		if err != nil {
			h.fail(w, r, err)
			return
		}
		done(w, r, a, pending)
	}
}

// headerWriter sets the download headers when the first byte goes out.
type headerWriter struct {
	w       http.ResponseWriter
	set     func(http.Header)
	started bool
}

func (hw *headerWriter) Write(b []byte) (int, error) {
	if !hw.started {
		hw.started = true
		hw.set(hw.w.Header())
		hw.w.WriteHeader(http.StatusOK)
	}
	return hw.w.Write(b)
}

// Export streams the signed-in account's export as a download. With reauth
// the body must carry the password and code (POST). An error before the
// first 64 KiB answers normally; a later one aborts the connection so a
// truncated file never looks complete.
func (h *Handlers) Export(reauth bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		a, f, ok := h.signedIn(w, r)
		if !ok {
			return
		}
		if reauth {
			if err := h.confirm(r.Context(), a, f, false); err != nil {
				h.fail(w, r, err)
				return
			}
		}
		e := h.Exporter
		now := e.now()
		ext := ".json"
		if e.Format == Zip {
			ext = ".zip"
		}
		name := "account-export" + ext
		if e.Filename != nil {
			name = e.Filename(a, now)
		}
		hw := &headerWriter{w: w, set: func(hd http.Header) {
			hd.Set("Content-Type", e.ContentType())
			hd.Set("Content-Disposition", `attachment; filename="`+SafeFilename(name)+`"`)
			hd.Set("Cache-Control", "no-store")
			hd.Set("X-Content-Type-Options", "nosniff")
		}}
		bw := bufio.NewWriterSize(hw, 64<<10)
		err := e.WriteTo(r.Context(), bw, a, now)
		if err == nil {
			err = bw.Flush()
		}
		if err == nil {
			return
		}
		if !hw.started {
			h.fail(w, r, err)
			return
		}
		h.log().Error("account export aborted", "err", err)
		panic(http.ErrAbortHandler)
	}
}

// DeleteOptions shape the self-service delete route.
type DeleteOptions struct {
	// ConfirmUsername requires the typed username (Fields.Confirm), compared
	// without case and surrounding space.
	ConfirmUsername bool
	// Reauth requires the password and code.
	Reauth bool
	// Done answers after the deletion, e.g. ends the request's cookie session
	// and reports Result; 204 when nil.
	Done func(w http.ResponseWriter, r *http.Request, d *Deletion)
}

func noContent(w http.ResponseWriter, _ *http.Request, _ *Deletion) {
	w.WriteHeader(http.StatusNoContent)
}

// DeleteAccount deletes the signed-in account.
func (h *Handlers) DeleteAccount(o DeleteOptions) http.HandlerFunc {
	if o.Done == nil {
		o.Done = noContent
	}
	return func(w http.ResponseWriter, r *http.Request) {
		a, f, ok := h.signedIn(w, r)
		if !ok {
			return
		}
		var err error
		if a.Admin && h.Deleter.Rule == KeepAdmins {
			err = ErrAdminSelfDelete
		}
		if err == nil && o.ConfirmUsername &&
			!strings.EqualFold(strings.TrimSpace(f[h.Fields.name(h.Fields.Confirm, "confirmUsername")]), a.Username) {
			err = ErrConfirmMismatch
		}
		if err == nil && o.Reauth {
			err = h.confirm(r.Context(), a, f, false)
		}
		var d *Deletion
		if err == nil {
			d, err = h.Deleter.Delete(r.Context(), a)
		}
		if err != nil {
			h.fail(w, r, err)
			return
		}
		o.Done(w, r, d)
	}
}

// AdminDeleteAccount deletes the account target names for the signed-in admin
// (the app's admin middleware runs first). done answers; 204 when nil.
func (h *Handlers) AdminDeleteAccount(target func(r *http.Request) (userid.ID, error), done func(w http.ResponseWriter, r *http.Request, d *Deletion)) http.HandlerFunc {
	if done == nil {
		done = noContent
	}
	return func(w http.ResponseWriter, r *http.Request) {
		actor, ok := h.Identify(r)
		if !ok || actor == "" {
			h.fail(w, r, ErrUnauthorized)
			return
		}
		id, err := target(r)
		if err != nil {
			h.fail(w, r, err)
			return
		}
		a, err := h.Directory.Account(r.Context(), id)
		var d *Deletion
		if err == nil {
			d, err = h.Deleter.DeleteByAdmin(r.Context(), actor, a)
		}
		if err != nil {
			h.fail(w, r, err)
			return
		}
		done(w, r, d)
	}
}

func (h *Handlers) writeSessions(w http.ResponseWriter, r *http.Request, a Account, extra map[string]any) {
	list, err := h.Sessions.List(r.Context(), a.ID, h.current(r))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	body := map[string]any{"sessions": list}
	for k, v := range extra {
		body[k] = v
	}
	httpx.WriteJSON(w, http.StatusOK, body)
}

// ListSessions answers {"sessions": [...]}.
func (h *Handlers) ListSessions() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		a, _, ok := h.signedIn(w, r)
		if ok {
			h.writeSessions(w, r, a, nil)
		}
	}
}

// RevokeSession ends the session named by the path value param and answers
// {"success": true, "sessions": [...]}.
func (h *Handlers) RevokeSession(param string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		a, _, ok := h.signedIn(w, r)
		if !ok {
			return
		}
		id := r.PathValue(param)
		if id == "" {
			h.fail(w, r, ErrSessionNotFound)
			return
		}
		if err := h.Sessions.Revoke(r.Context(), a.ID, id, h.current(r)); err != nil {
			h.fail(w, r, err)
			return
		}
		h.writeSessions(w, r, a, map[string]any{"success": true})
	}
}

// SignOutEverywhere ends the account's sessions. keepCurrent spares the
// caller's and answers {"success": true, "sessions": [...]}; otherwise every
// session ends, and done (204 when nil) answers, e.g. clearing the cookie.
func (h *Handlers) SignOutEverywhere(keepCurrent bool, done func(w http.ResponseWriter, r *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		a, _, ok := h.signedIn(w, r)
		if !ok {
			return
		}
		keep := ""
		if keepCurrent {
			keep = h.current(r)
		}
		if err := h.Sessions.RevokeAll(r.Context(), a.ID, keep); err != nil {
			h.fail(w, r, err)
			return
		}
		switch {
		case keepCurrent:
			h.writeSessions(w, r, a, map[string]any{"success": true})
		case done != nil:
			done(w, r)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}
}
