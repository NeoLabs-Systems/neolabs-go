// Package oauthlogin signs people in (and registers or links accounts) with an
// external identity provider through the OAuth 2 authorization-code flow with
// PKCE. It owns the pending-flow state (auth_oauth_states) and the provider
// links (user_auth_providers); creating the account row is the app's job.
package oauthlogin

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/NeoLabs-Systems/neolabs-go/apperr"
	"github.com/NeoLabs-Systems/neolabs-go/dbx"
	"github.com/NeoLabs-Systems/neolabs-go/secretbox"
)

const stateTTL = 15 * time.Minute

// Identity is who the provider says signed in. JSON names are the ones stored
// by the Node servers in auth_oauth_states.result_json.
type Identity struct {
	ProviderUserID string         `json:"providerUserId"`
	Email          string         `json:"email"`
	EmailVerified  bool           `json:"emailVerified"`
	DisplayName    string         `json:"displayName,omitempty"`
	Username       string         `json:"username,omitempty"`
	AvatarURL      *string        `json:"avatarUrl,omitempty"`
	Metadata       map[string]any `json:"metadata,omitempty"`
	// Secret carries a credential some providers hand out at sign-in (an API
	// key); it is kept sealed and passed to SaveCredentials.
	Secret string `json:"apiKey,omitempty"`
}

// Provider is one identity provider.
type Provider interface {
	Key() string
	Label() string
	Icon() string
	// Status says whether sign-in is set up for the server at baseURL.
	Status(baseURL string) (configured bool, summary string)
	// Begin returns the address the person is sent to.
	Begin(ctx context.Context, state, codeVerifier, baseURL string) (string, error)
	// Finish exchanges the callback code for an identity.
	Finish(ctx context.Context, code, codeVerifier string) (Identity, error)
}

// Gated is implemented by providers an administrator can switch off.
type Gated interface{ AssertEnabled() error }

// CredentialSaver is implemented by providers that store a credential at sign-in.
type CredentialSaver interface {
	SaveCredentials(ctx context.Context, userID int64, id Identity) error
}

// Accounts is what the engine needs from the app's account store.
type Accounts interface {
	SignupKeyRequired(ctx context.Context) (bool, error)
	AssertSignupKeyUsable(ctx context.Context, raw string) (string, error)
	ValidateNewUsername(ctx context.Context, username string) error
	EmailTaken(ctx context.Context, email string) (bool, error)
	// CreateFromProvider creates the account and its provider link atomically
	// (spending the sign-up key), or returns the existing link's account.
	CreateFromProvider(ctx context.Context, p Provider, id Identity, signupKey, username string) (userID int64, created bool, err error)
}

// Engine runs the flows.
type Engine struct {
	DB        *dbx.DB
	Box       *secretbox.Box // seals the PKCE verifier and held identities
	Providers []Provider
	Accounts  Accounts
	Product   string // names the product in messages, e.g. "NeoAgent"
	Now       func() time.Time
}

func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

// Provider returns a provider by key, or nil.
func (e *Engine) Provider(key string) Provider {
	key = strings.TrimSpace(key)
	for _, p := range e.Providers {
		if p.Key() == key {
			return p
		}
	}
	return nil
}

func isoNow(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

// Info is a provider as the sign-in screen lists it.
type Info struct {
	ID         string `json:"id"`
	Label      string `json:"label"`
	Icon       string `json:"icon"`
	Configured bool   `json:"configured"`
	Summary    string `json:"summary"`
}

func (e *Engine) cleanup(ctx context.Context) {
	// Housekeeping must never fail a request.
	_, _ = e.DB.W.ExecContext(ctx, `DELETE FROM auth_oauth_states WHERE datetime(expires_at) <= datetime('now')
		OR status IN ('completed','failed') AND datetime(created_at) <= datetime('now','-1 day')`)
}

// List returns the providers with their setup status.
func (e *Engine) List(ctx context.Context, baseURL string) []Info {
	e.cleanup(ctx)
	out := make([]Info, 0, len(e.Providers))
	for _, p := range e.Providers {
		ok, summary := p.Status(baseURL)
		icon := p.Icon()
		if icon == "" {
			icon = p.Key()
		}
		out = append(out, Info{ID: p.Key(), Label: p.Label(), Icon: icon, Configured: ok, Summary: summary})
	}
	return out
}

// Linked is a provider linked to an account.
type Linked struct {
	ID         int64          `json:"id"`
	Provider   string         `json:"provider"`
	Label      string         `json:"label"`
	Icon       string         `json:"icon"`
	Email      *string        `json:"email"`
	Metadata   map[string]any `json:"metadata"`
	LastUsedAt *string        `json:"lastUsedAt"`
	LinkedAt   *string        `json:"linkedAt"`
	CanUnlink  bool           `json:"canUnlink"`
}

func strPtr(n sql.NullString) *string {
	if !n.Valid || n.String == "" {
		return nil
	}
	return &n.String
}

func jsonObject(raw string) map[string]any {
	out := map[string]any{}
	if raw != "" {
		_ = json.Unmarshal([]byte(raw), &out)
		if out == nil {
			out = map[string]any{}
		}
	}
	return out
}

// ListForUser returns the providers linked to the account.
func (e *Engine) ListForUser(ctx context.Context, userID int64) ([]Linked, error) {
	var pw sql.NullInt64
	err := e.DB.R.QueryRowContext(ctx, `SELECT password_login_enabled FROM users WHERE id = ?`, userID).Scan(&pw)
	if errors.Is(err, sql.ErrNoRows) {
		return []Linked{}, nil
	}
	if err != nil {
		return nil, err
	}
	rows, err := e.DB.R.QueryContext(ctx, `SELECT id, provider_key, email, metadata_json, last_used_at, created_at
		FROM user_auth_providers WHERE user_id = ? ORDER BY provider_key ASC, updated_at DESC, id DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []Linked
	for rows.Next() {
		var l Linked
		var email, meta, used, created sql.NullString
		if err := rows.Scan(&l.ID, &l.Provider, &email, &meta, &used, &created); err != nil {
			return nil, err
		}
		l.Email, l.Metadata = strPtr(email), jsonObject(meta.String)
		l.LastUsedAt, l.LinkedAt = strPtr(used), strPtr(created)
		list = append(list, l)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	canFallBack := pw.Int64 == 1
	for i := range list {
		list[i].Label, list[i].Icon = list[i].Provider, list[i].Provider
		if p := e.Provider(list[i].Provider); p != nil {
			list[i].Label = p.Label()
			if icon := p.Icon(); icon != "" {
				list[i].Icon = icon
			}
		}
		list[i].CanUnlink = canFallBack || len(list) > 1
	}
	if list == nil {
		list = []Linked{}
	}
	return list, nil
}

// BeginParams start a flow.
type BeginParams struct {
	ProviderKey string
	Mode        string // login, register or link
	UserID      int64  // required for link
	SignupKey   string
	BaseURL     string
}

// Started is the answer to Begin.
type Started struct {
	Provider string `json:"provider"`
	Mode     string `json:"mode"`
	Status   string `json:"status"`
	State    string `json:"state"`
	URL      string `json:"url"`
}

func normalizeMode(v string) string {
	switch m := strings.ToLower(strings.TrimSpace(v)); m {
	case "login", "register", "link":
		return m
	}
	return ""
}

func plain(msg string) error { return apperr.New(400, msg, "") }

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("oauthlogin: crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// CodeChallenge is the PKCE S256 challenge of a verifier.
func CodeChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// Begin starts a flow and records its state.
func (e *Engine) Begin(ctx context.Context, p BeginParams) (*Started, error) {
	e.cleanup(ctx)
	prov := e.Provider(p.ProviderKey)
	if prov == nil {
		return nil, plain("Unknown sign-in provider: " + p.ProviderKey)
	}
	mode := normalizeMode(p.Mode)
	if mode == "" {
		return nil, plain("A valid provider auth mode is required.")
	}
	if mode == "link" && p.UserID == 0 {
		return nil, plain("You must be signed in to link a provider.")
	}
	// Checked here so a bad key fails before the provider round trip; it is
	// spent only when the callback creates the account.
	var keyCode any
	if mode == "register" {
		required, err := e.Accounts.SignupKeyRequired(ctx)
		if err != nil {
			return nil, err
		}
		if required {
			code, err := e.Accounts.AssertSignupKeyUsable(ctx, p.SignupKey)
			if err != nil {
				return nil, err
			}
			keyCode = code
		}
	}
	if ok, summary := prov.Status(p.BaseURL); !ok {
		return nil, plain(summary)
	}

	state := "auth_" + randHex(24)
	verifierRaw := make([]byte, 48)
	if _, err := rand.Read(verifierRaw); err != nil {
		return nil, err
	}
	verifier := base64.RawURLEncoding.EncodeToString(verifierRaw)
	url, err := prov.Begin(ctx, state, verifier, p.BaseURL)
	if err != nil {
		return nil, plain(err.Error())
	}
	sealed, err := e.Box.Seal(verifier)
	if err != nil {
		return nil, err
	}
	var user any
	if p.UserID != 0 {
		user = p.UserID
	}
	if _, err := e.DB.W.ExecContext(ctx, `INSERT INTO auth_oauth_states (user_id, provider_key, mode, state, code_verifier, expires_at, signup_key)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, user, prov.Key(), mode, state, sealed, isoNow(e.now().Add(stateTTL)), keyCode); err != nil {
		return nil, err
	}
	return &Started{Provider: prov.Key(), Mode: mode, Status: "oauth_redirect", State: state, URL: url}, nil
}

type stateRow struct {
	id                               int64
	userID                           sql.NullInt64
	providerKey, mode, state         string
	codeVerifier, status             string
	resultJSON, signupKey, createdAt sql.NullString
	errMsg                           sql.NullString
}

const stateColumns = `id, user_id, provider_key, mode, state, code_verifier, COALESCE(status,'pending'), result_json, signup_key, COALESCE(created_at,''), error_message`

func scanState(r interface{ Scan(...any) error }) (*stateRow, error) {
	var s stateRow
	err := r.Scan(&s.id, &s.userID, &s.providerKey, &s.mode, &s.state, &s.codeVerifier, &s.status, &s.resultJSON, &s.signupKey, &s.createdAt, &s.errMsg)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &s, err
}

// pendingState is a live, still-pending state row.
func (e *Engine) pendingState(ctx context.Context, state string) (*stateRow, error) {
	r, err := scanState(e.DB.W.QueryRowContext(ctx, `SELECT `+stateColumns+` FROM auth_oauth_states
		WHERE state = ? AND datetime(expires_at) > datetime('now')`, strings.TrimSpace(state)))
	if err != nil || r == nil || r.status != "pending" {
		return nil, err
	}
	return r, nil
}

// Fail marks a pending flow as failed (the person declined at the provider).
func (e *Engine) Fail(ctx context.Context, state, message string) error {
	r, err := e.pendingState(ctx, state)
	if err != nil || r == nil {
		return err
	}
	if message == "" {
		message = "Authentication failed."
	}
	_, err = e.DB.W.ExecContext(ctx, `UPDATE auth_oauth_states SET status = 'failed', error_message = ?, completed_at = ? WHERE id = ?`,
		message, isoNow(e.now()), r.id)
	return err
}

// Pending describes what a person is approving when a held callback asks for
// confirmation, or nil when the flow is unknown or finished.
type Pending struct {
	ProviderLabel string
	Mode          string
	Username      string
	StartedAt     string
}

// Describe returns the pending flow's description.
func (e *Engine) Describe(ctx context.Context, state string) (*Pending, error) {
	r, err := e.pendingState(ctx, state)
	if err != nil || r == nil {
		return nil, err
	}
	p := &Pending{ProviderLabel: r.providerKey, Mode: r.mode, StartedAt: r.createdAt.String}
	if prov := e.Provider(r.providerKey); prov != nil {
		p.ProviderLabel = prov.Label()
	}
	if r.userID.Valid {
		_ = e.DB.R.QueryRowContext(ctx, `SELECT username FROM users WHERE id = ?`, r.userID.Int64).Scan(&p.Username)
	}
	return p, nil
}

// Result is what a finished flow resolved to; JSON names match the Node servers.
type Result struct {
	Action           string `json:"action"`
	UserID           int64  `json:"userId,omitempty"`
	Provider         string `json:"provider"`
	ProviderLabel    string `json:"providerLabel,omitempty"`
	Email            string `json:"email,omitempty"`
	Created          *bool  `json:"created,omitempty"`
	RequiresUsername bool   `json:"requiresUsername,omitempty"`
}

// Finish completes a flow from the provider callback.
func (e *Engine) Finish(ctx context.Context, state, code string) (*Result, error) {
	r, err := e.pendingState(ctx, state)
	if err != nil {
		return nil, err
	}
	if r == nil {
		return nil, plain("OAuth state is missing or expired.")
	}
	prov := e.Provider(r.providerKey)
	if prov == nil {
		return nil, plain("Unknown sign-in provider: " + r.providerKey)
	}
	res, err := e.DB.W.ExecContext(ctx, `UPDATE auth_oauth_states SET status = 'exchanging' WHERE id = ? AND status = 'pending'`, r.id)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, plain("OAuth state is missing or expired.")
	}
	verifier, err := e.Box.Open(r.codeVerifier)
	if err != nil {
		return nil, e.failState(ctx, r.id, err)
	}
	identity, err := prov.Finish(ctx, code, verifier)
	if err != nil {
		return nil, e.failState(ctx, r.id, err)
	}
	result, err := e.resolve(ctx, r, prov, identity, "")
	if err != nil {
		return nil, e.failState(ctx, r.id, err)
	}
	if result.RequiresUsername {
		raw, _ := json.Marshal(identity)
		sealed, err := e.Box.Seal(string(raw))
		if err != nil {
			return nil, e.failState(ctx, r.id, err)
		}
		_, err = e.DB.W.ExecContext(ctx, `UPDATE auth_oauth_states SET status = 'requires_username', result_json = ? WHERE id = ?`, sealed, r.id)
		return result, err
	}
	raw, _ := json.Marshal(result)
	_, err = e.DB.W.ExecContext(ctx, `UPDATE auth_oauth_states SET status = 'completed', result_json = ?, error_message = NULL, completed_at = ? WHERE id = ?`,
		string(raw), isoNow(e.now()), r.id)
	return result, err
}

func (e *Engine) failState(ctx context.Context, id int64, cause error) error {
	msg := cause.Error()
	if msg == "" {
		msg = "Authentication failed."
	}
	_, _ = e.DB.W.ExecContext(ctx, `UPDATE auth_oauth_states SET status = 'failed', error_message = ?, completed_at = ? WHERE id = ?`,
		msg, isoNow(e.now()), id)
	return cause
}

// Completion is what the client collects once the flow ends.
type Completion struct {
	Status           string
	Mode             string
	Provider         string
	Result           map[string]any
	RequiresUsername bool
}

// Consume returns the flow's outcome to the client that started it and removes
// it. A flow still in progress reports "pending"; one awaiting a username keeps
// its state so the client can finish registration.
func (e *Engine) Consume(ctx context.Context, state string) (*Completion, error) {
	e.cleanup(ctx)
	r, err := scanState(e.DB.W.QueryRowContext(ctx, `SELECT `+stateColumns+` FROM auth_oauth_states WHERE state = ?`, strings.TrimSpace(state)))
	if err != nil {
		return nil, err
	}
	if r == nil {
		return nil, apperr.New(404, "Authentication request was not found or has expired.", "")
	}
	switch r.status {
	case "pending", "exchanging":
		return &Completion{Status: "pending", Mode: r.mode, Provider: r.providerKey}, nil
	case "requires_username":
		if err := e.assertEnabled(r.providerKey); err != nil {
			return nil, err
		}
		return &Completion{Status: "requires_username", RequiresUsername: true, Provider: r.providerKey}, nil
	}
	if _, err := e.DB.W.ExecContext(ctx, `DELETE FROM auth_oauth_states WHERE id = ?`, r.id); err != nil {
		return nil, err
	}
	if r.status == "failed" {
		return nil, plain(firstNonEmpty(r.errMsg.String, "Authentication failed."))
	}
	if err := e.assertEnabled(r.providerKey); err != nil {
		return nil, err
	}
	return &Completion{Status: "completed", Mode: r.mode, Provider: r.providerKey, Result: jsonObject(r.resultJSON.String)}, nil
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

func (e *Engine) assertEnabled(key string) error {
	if g, ok := e.Provider(key).(Gated); ok {
		if err := g.AssertEnabled(); err != nil {
			return plain(err.Error())
		}
	}
	return nil
}

// CompleteRegistration finishes a registration that needed the person to pick a username.
func (e *Engine) CompleteRegistration(ctx context.Context, state, username string) error {
	e.cleanup(ctx)
	r, err := scanState(e.DB.W.QueryRowContext(ctx, `SELECT `+stateColumns+` FROM auth_oauth_states WHERE state = ? AND status = 'requires_username'`, strings.TrimSpace(state)))
	if err != nil {
		return err
	}
	if r == nil {
		return apperr.New(404, "Registration request was not found or has expired.", "")
	}
	prov := e.Provider(r.providerKey)
	if prov == nil {
		return plain("Unknown sign-in provider: " + r.providerKey)
	}
	if err := e.assertEnabled(r.providerKey); err != nil {
		return err
	}
	chosen := strings.TrimSpace(username)
	if err := e.Accounts.ValidateNewUsername(ctx, chosen); err != nil {
		return err
	}
	held, err := e.Box.OpenSealed(r.resultJSON.String)
	if err != nil {
		return plain("Registration request was not found or has expired.")
	}
	var identity Identity
	if err := json.Unmarshal([]byte(held), &identity); err != nil {
		return plain("Registration request was not found or has expired.")
	}
	if _, err := e.DB.W.ExecContext(ctx, `UPDATE auth_oauth_states SET status = 'exchanging' WHERE id = ?`, r.id); err != nil {
		return err
	}
	result, err := e.resolve(ctx, r, prov, identity, chosen)
	if err != nil {
		retryable := false
		if ae, ok := apperr.As(err); ok {
			retryable = ae.Code == "USERNAME_TAKEN" || ae.Code == "USERNAME_TOO_SHORT"
		}
		next := "failed"
		if retryable {
			next = "requires_username"
		}
		_, _ = e.DB.W.ExecContext(ctx, `UPDATE auth_oauth_states SET status = ?, error_message = ? WHERE id = ?`, next, err.Error(), r.id)
		return err
	}
	raw, _ := json.Marshal(result)
	_, err = e.DB.W.ExecContext(ctx, `UPDATE auth_oauth_states SET status = 'completed', result_json = ?, completed_at = ? WHERE id = ?`,
		string(raw), isoNow(e.now()), r.id)
	return err
}

// Unlink removes a linked provider, refusing to remove the last way to sign in.
func (e *Engine) Unlink(ctx context.Context, userID, linkID int64) (removed bool, err error) {
	if linkID <= 0 {
		return false, plain("A valid linked provider id is required.")
	}
	var one int
	err = e.DB.W.QueryRowContext(ctx, `SELECT 1 FROM user_auth_providers WHERE id = ? AND user_id = ?`, linkID, userID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var pw sql.NullInt64
	_ = e.DB.W.QueryRowContext(ctx, `SELECT password_login_enabled FROM users WHERE id = ?`, userID).Scan(&pw)
	var linked int
	if err := e.DB.W.QueryRowContext(ctx, `SELECT COUNT(*) FROM user_auth_providers WHERE user_id = ?`, userID).Scan(&linked); err != nil {
		return false, err
	}
	if pw.Int64 != 1 && linked <= 1 {
		return false, plain("Create a password or link another provider before removing this sign-in method.")
	}
	_, err = e.DB.W.ExecContext(ctx, `DELETE FROM user_auth_providers WHERE id = ? AND user_id = ?`, linkID, userID)
	return err == nil, err
}

// Execer runs SQL inside or outside a transaction.
type Execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// SaveLink records (or refreshes) the link between an account and a provider identity.
func SaveLink(ctx context.Context, q Execer, userID int64, providerKey string, id Identity) error {
	meta := map[string]any{"displayName": nilIfEmpty(id.DisplayName), "avatarUrl": id.AvatarURL, "emailVerified": id.EmailVerified}
	for k, v := range id.Metadata {
		meta[k] = v
	}
	raw, _ := json.Marshal(meta)
	var email any
	if id.Email != "" {
		email = strings.ToLower(id.Email)
	}
	_, err := q.ExecContext(ctx, `INSERT INTO user_auth_providers (user_id, provider_key, provider_user_id, email, metadata_json, last_used_at, updated_at)
		VALUES (?, ?, ?, ?, ?, datetime('now'), datetime('now'))
		ON CONFLICT(provider_key, provider_user_id) DO UPDATE SET
		  user_id = excluded.user_id, email = excluded.email, metadata_json = excluded.metadata_json,
		  last_used_at = excluded.last_used_at, updated_at = excluded.updated_at`,
		userID, providerKey, id.ProviderUserID, email, string(raw))
	return err
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// saveAccount links an existing account and stores any provider credential.
func (e *Engine) saveAccount(ctx context.Context, userID int64, prov Provider, id Identity) error {
	if err := SaveLink(ctx, e.DB.W, userID, prov.Key(), id); err != nil {
		return err
	}
	if s, ok := prov.(CredentialSaver); ok {
		return s.SaveCredentials(ctx, userID, id)
	}
	return nil
}

func (e *Engine) resolve(ctx context.Context, st *stateRow, prov Provider, id Identity, username string) (*Result, error) {
	var linkedUser int64
	err := e.DB.W.QueryRowContext(ctx, `SELECT user_id FROM user_auth_providers WHERE provider_key = ? AND provider_user_id = ?`,
		prov.Key(), id.ProviderUserID).Scan(&linkedUser)
	linked := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	base := Result{Provider: prov.Key(), ProviderLabel: prov.Label(), Email: id.Email}

	switch st.mode {
	case "login":
		if !linked {
			return nil, plain(fmt.Sprintf("No %s account is linked to this %s account. Register first or sign in and link it from account settings.", e.Product, prov.Label()))
		}
		if err := e.saveAccount(ctx, linkedUser, prov, id); err != nil {
			return nil, err
		}
		base.Action, base.UserID = "login", linkedUser
		return &base, nil
	case "link":
		if !st.userID.Valid {
			return nil, plain("You must be signed in to link a provider.")
		}
		if linked && linkedUser != st.userID.Int64 {
			return nil, plain(fmt.Sprintf("This %s account is already linked to another %s account.", prov.Label(), e.Product))
		}
		if err := e.saveAccount(ctx, st.userID.Int64, prov, id); err != nil {
			return nil, err
		}
		base.Action, base.UserID = "link", st.userID.Int64
		return &base, nil
	}

	created := false
	if linked {
		if err := e.saveAccount(ctx, linkedUser, prov, id); err != nil {
			return nil, err
		}
		base.Action, base.UserID, base.Created = "register", linkedUser, &created
		return &base, nil
	}
	if id.Email != "" {
		taken, err := e.Accounts.EmailTaken(ctx, id.Email)
		if err != nil {
			return nil, err
		}
		if taken {
			return nil, plain("That email already belongs to an existing account. Sign in first and link this provider from account settings.")
		}
	}
	if username == "" && !HasUsernameCandidate(id) {
		return &Result{Action: "register", Provider: prov.Key(), RequiresUsername: true}, nil
	}
	var signupKey string
	if st.signupKey.Valid {
		signupKey = st.signupKey.String
	}
	userID, wasCreated, err := e.Accounts.CreateFromProvider(ctx, prov, id, signupKey, username)
	if err != nil {
		return nil, err
	}
	if s, ok := prov.(CredentialSaver); ok && wasCreated {
		if err := s.SaveCredentials(ctx, userID, id); err != nil {
			return nil, err
		}
	}
	base.Action, base.UserID, base.Created = "register", userID, &wasCreated
	return &base, nil
}

// UsernameCandidate turns a display name or email local part into a username:
// lower case, [a-z0-9._-] only, no leading or trailing punctuation, 3-32 chars.
// It returns "" when nothing usable is left.
func UsernameCandidate(value string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(value)) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-' {
			b.WriteRune(r)
		}
	}
	s := strings.Trim(b.String(), "._-")
	if len(s) < 3 {
		return ""
	}
	if len(s) > 32 {
		s = s[:32]
	}
	return s
}

// HasUsernameCandidate reports whether the identity offers a usable username.
func HasUsernameCandidate(id Identity) bool {
	local := id.Email
	if i := strings.Index(local, "@"); i >= 0 {
		local = local[:i]
	}
	return UsernameCandidate(firstNonEmpty(id.Username, id.DisplayName)) != "" || UsernameCandidate(local) != ""
}
