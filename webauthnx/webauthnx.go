// Package webauthnx runs passkey / security-key ceremonies for the NeoLabs
// apps. It wraps go-webauthn and keeps the behaviour of the Node servers: the
// relying party follows the origin the ceremony runs on, credentials are
// stored in the same table layout (base64url ids and COSE public keys), and a
// user-verified key stands in for the second factor.
//
// Storage and accounts are interfaces, so each app keeps its own schema.
package webauthnx

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/NeoLabs-Systems/neolabs-go/httpx"
	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
)

const (
	challengeTTL    = 5 * time.Minute
	ceremonyTimeout = 2 * time.Minute
	maxLabelLength  = 48
)

// Credential is a stored credential row.
type Credential struct {
	RowID        int64
	UserID       string
	CredentialID string // base64url
	PublicKey    string // base64url COSE key
	Counter      uint32
	RPID         string
	Transports   []string
	DeviceType   string // "singleDevice" or "multiDevice"
	BackedUp     bool
	UserVerified bool
	Label        string
	CreatedAt    string
	LastUsedAt   string
}

// Payload is the JSON the clients list.
type Payload struct {
	ID         int64    `json:"id"`
	Label      string   `json:"label"`
	CreatedAt  *string  `json:"createdAt"`
	LastUsedAt *string  `json:"lastUsedAt"`
	Transports []string `json:"transports"`
	BackedUp   bool     `json:"backedUp"`
	RPID       string   `json:"rpId"`
}

func (c Credential) Payload() Payload {
	p := Payload{ID: c.RowID, Label: c.Label, Transports: c.Transports, BackedUp: c.BackedUp, RPID: c.RPID}
	if p.Transports == nil {
		p.Transports = []string{}
	}
	if c.CreatedAt != "" {
		p.CreatedAt = &c.CreatedAt
	}
	if c.LastUsedAt != "" {
		p.LastUsedAt = &c.LastUsedAt
	}
	return p
}

// Store persists credentials.
type Store interface {
	ListForUser(ctx context.Context, userID string) ([]Credential, error)
	ForUserAndRP(ctx context.Context, userID, rpID string) ([]Credential, error)
	CountForRP(ctx context.Context, rpID string) (int, error)
	FindByCredentialID(ctx context.Context, credentialID, rpID string) (*Credential, error)
	Exists(ctx context.Context, credentialID string) (bool, error)
	Insert(ctx context.Context, c Credential) error
	UpdateAfterLogin(ctx context.Context, rowID int64, counter uint32, userVerified bool) error
	Rename(ctx context.Context, userID string, rowID int64, label string) (bool, error)
	Delete(ctx context.Context, userID string, rowID int64) (bool, error)
}

// Identity is what a ceremony needs to know about an account.
type Identity struct {
	ID          string
	Username    string
	DisplayName string
	Email       string
}

// Accounts looks accounts up.
type Accounts interface {
	// ByID returns the account for registration, or ok=false.
	ByID(ctx context.Context, userID string) (id Identity, ok bool, err error)
	// ForSignIn resolves a username or email, or ok=false.
	ForSignIn(ctx context.Context, identifier string) (id Identity, ok bool, err error)
}

// Error is a ceremony failure with the HTTP status clients receive.
type Error struct {
	Status int
	// Code is stable; apps match on it to use their own wording.
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Message }

func fail(status int, code, msg string) *Error {
	return &Error{Status: status, Code: code, Message: msg}
}

// Pending is the state kept in the session between begin and complete.
type Pending struct {
	Session   webauthn.SessionData `json:"session"`
	RPID      string               `json:"rpId"`
	ExpiresAt int64                `json:"expiresAt"`
}

// Service runs the ceremonies.
type Service struct {
	RPName     string
	Origins    httpx.Origins
	TrustProxy bool
	MaxPerUser int // default 20
	Store      Store
	Accounts   Accounts
	Now        func() time.Time
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Service) maxPerUser() int {
	if s.MaxPerUser > 0 {
		return s.MaxPerUser
	}
	return 20
}

// relyingParty derives the relying party from the origin the ceremony runs on.
func (s *Service) relyingParty(r *http.Request) (origin, rpID string, err error) {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	} else if s.TrustProxy {
		first := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Proto"), ",")[0])
		if strings.EqualFold(first, "https") {
			scheme = "https"
		}
	}
	self := scheme + "://" + r.Host
	origin = strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		origin = self
	}
	if origin != self && !s.Origins.Allowed(origin, false) {
		return "", "", fail(403, "ORIGIN_NOT_ALLOWED", "Security key sign-in is not allowed from this origin.")
	}
	u, perr := url.Parse(origin)
	if perr != nil || u.Hostname() == "" {
		return "", "", fail(400, "INVALID_ORIGIN", "Security key sign-in requires a valid request origin.")
	}
	return origin, u.Hostname(), nil
}

func (s *Service) client(origin, rpID string) (*webauthn.WebAuthn, error) {
	return webauthn.New(&webauthn.Config{
		RPID:                  rpID,
		RPDisplayName:         s.RPName,
		RPOrigins:             []string{origin},
		AttestationPreference: protocol.PreferNoAttestation,
		Timeouts: webauthn.TimeoutsConfig{
			Login:        webauthn.TimeoutConfig{Enforce: true, Timeout: ceremonyTimeout, TimeoutUVD: ceremonyTimeout},
			Registration: webauthn.TimeoutConfig{Enforce: true, Timeout: ceremonyTimeout, TimeoutUVD: ceremonyTimeout},
		},
	})
}

func (s *Service) readPending(p *Pending, rpID string) (*Pending, error) {
	if p == nil || p.Session.Challenge == "" || s.now().UnixMilli() > p.ExpiresAt {
		return nil, fail(400, "SECURITY_KEY_CHALLENGE_EXPIRED", "Security key challenge expired, start again.")
	}
	if p.RPID != rpID {
		return nil, fail(400, "SECURITY_KEY_CHALLENGE_EXPIRED", "Security key challenge was started on a different address.")
	}
	return p, nil
}

func (s *Service) newPending(session *webauthn.SessionData, rpID string) *Pending {
	return &Pending{Session: *session, RPID: rpID, ExpiresAt: s.now().Add(challengeTTL).UnixMilli()}
}

func descriptors(creds []Credential) []protocol.CredentialDescriptor {
	out := make([]protocol.CredentialDescriptor, 0, len(creds))
	for _, c := range creds {
		id, err := base64.RawURLEncoding.DecodeString(c.CredentialID)
		if err != nil {
			continue
		}
		d := protocol.CredentialDescriptor{Type: protocol.PublicKeyCredentialType, CredentialID: id}
		for _, t := range c.Transports {
			d.Transport = append(d.Transport, protocol.AuthenticatorTransport(t))
		}
		out = append(out, d)
	}
	return out
}

// user adapts an account to go-webauthn.
type user struct {
	id          string
	name        string
	displayName string
	credentials []webauthn.Credential
}

func (u user) WebAuthnID() []byte                         { return []byte(u.id) }
func (u user) WebAuthnName() string                       { return u.name }
func (u user) WebAuthnDisplayName() string                { return u.displayName }
func (u user) WebAuthnCredentials() []webauthn.Credential { return u.credentials }

func normalizeLabel(value, fallback string) (string, error) {
	label := strings.Join(strings.Fields(value), " ")
	if label == "" {
		return fallback, nil
	}
	if len([]rune(label)) > maxLabelLength {
		return "", fail(400, "SECURITY_KEY_NAME_TOO_LONG", fmt.Sprintf("Security key name must be %d characters or fewer.", maxLabelLength))
	}
	return label, nil
}

// List returns a user's keys in client form.
func (s *Service) List(ctx context.Context, userID string) ([]Payload, error) {
	creds, err := s.Store.ListForUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := make([]Payload, len(creds))
	for i, c := range creds {
		out[i] = c.Payload()
	}
	return out, nil
}

// BeginRegistration starts adding a key. The returned options are sent to the
// browser; the pending state goes into the session.
func (s *Service) BeginRegistration(ctx context.Context, r *http.Request, userID string) (any, *Pending, error) {
	origin, rpID, err := s.relyingParty(r)
	if err != nil {
		return nil, nil, err
	}
	id, ok, err := s.Accounts.ByID(ctx, userID)
	if err != nil {
		return nil, nil, err
	}
	if !ok {
		return nil, nil, fail(404, "NOT_FOUND", "User not found")
	}
	all, err := s.Store.ListForUser(ctx, userID)
	if err != nil {
		return nil, nil, err
	}
	if len(all) >= s.maxPerUser() {
		return nil, nil, fail(400, "SECURITY_KEY_LIMIT", fmt.Sprintf("You can register at most %d security keys.", s.maxPerUser()))
	}
	existing, err := s.Store.ForUserAndRP(ctx, userID, rpID)
	if err != nil {
		return nil, nil, err
	}
	wa, err := s.client(origin, rpID)
	if err != nil {
		return nil, nil, err
	}
	name := id.Email
	if name == "" {
		name = id.Username
	}
	display := id.DisplayName
	if display == "" {
		display = id.Username
	}
	creation, session, err := wa.BeginRegistration(
		user{id: id.ID, name: name, displayName: display},
		webauthn.WithExclusions(descriptors(existing)),
		webauthn.WithAuthenticatorSelection(protocol.AuthenticatorSelection{
			ResidentKey:      protocol.ResidentKeyRequirementPreferred,
			UserVerification: protocol.VerificationPreferred,
		}),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("webauthnx: begin registration: %w", err)
	}
	return creation.Response, s.newPending(session, rpID), nil
}

// CompleteRegistration verifies the browser's answer and stores the key.
func (s *Service) CompleteRegistration(ctx context.Context, r *http.Request, userID string, response json.RawMessage, label string, pending *Pending) error {
	origin, rpID, err := s.relyingParty(r)
	if err != nil {
		return err
	}
	p, err := s.readPending(pending, rpID)
	if err != nil {
		return err
	}
	wa, err := s.client(origin, rpID)
	if err != nil {
		return err
	}
	parsed, perr := protocol.ParseCredentialCreationResponseBytes(response)
	if perr != nil {
		return fail(400, "SECURITY_KEY_REJECTED", "Security key could not be verified.")
	}
	cred, verr := wa.CreateCredential(user{id: userID}, p.Session, parsed)
	if verr != nil {
		return fail(400, "SECURITY_KEY_REJECTED", "Security key could not be verified.")
	}

	credID := base64.RawURLEncoding.EncodeToString(cred.ID)
	if exists, err := s.Store.Exists(ctx, credID); err != nil {
		return err
	} else if exists {
		return fail(409, "SECURITY_KEY_EXISTS", "That security key is already registered.")
	}
	existing, err := s.Store.ListForUser(ctx, userID)
	if err != nil {
		return err
	}
	name, err := normalizeLabel(label, fmt.Sprintf("Security key %d", len(existing)+1))
	if err != nil {
		return err
	}
	deviceType := "singleDevice"
	if cred.Flags.BackupEligible {
		deviceType = "multiDevice"
	}
	transports := make([]string, 0, len(cred.Transport))
	for _, t := range cred.Transport {
		transports = append(transports, string(t))
	}
	return s.Store.Insert(ctx, Credential{
		UserID:       userID,
		CredentialID: credID,
		PublicKey:    base64.RawURLEncoding.EncodeToString(cred.PublicKey),
		Counter:      cred.Authenticator.SignCount,
		RPID:         rpID,
		Transports:   transports,
		DeviceType:   deviceType,
		BackedUp:     cred.Flags.BackupState,
		UserVerified: cred.Flags.UserVerified,
		Label:        name,
	})
}

// BeginLogin starts a sign-in. With a username the browser is steered to that
// account's keys; without one it offers any discoverable credential.
func (s *Service) BeginLogin(ctx context.Context, r *http.Request, username string) (any, *Pending, error) {
	origin, rpID, err := s.relyingParty(r)
	if err != nil {
		return nil, nil, err
	}
	// With nothing registered anywhere the browser would sit on its key prompt
	// until the ceremony times out. The count covers the whole relying party, so
	// refusing early says nothing about any particular account.
	n, err := s.Store.CountForRP(ctx, rpID)
	if err != nil {
		return nil, nil, err
	}
	if n == 0 {
		return nil, nil, fail(409, "NO_SECURITY_KEYS", "No security keys are registered for this server yet. Add one under Account settings first.")
	}
	opts := []webauthn.LoginOption{webauthn.WithUserVerification(protocol.VerificationPreferred)}
	if account := strings.TrimSpace(username); account != "" {
		id, ok, err := s.Accounts.ForSignIn(ctx, account)
		if err != nil {
			return nil, nil, err
		}
		if ok {
			creds, err := s.Store.ForUserAndRP(ctx, id.ID, rpID)
			if err != nil {
				return nil, nil, err
			}
			if allow := descriptors(creds); len(allow) > 0 {
				opts = append(opts, webauthn.WithAllowedCredentials(allow))
			}
		}
	}
	wa, err := s.client(origin, rpID)
	if err != nil {
		return nil, nil, err
	}
	assertion, session, err := wa.BeginDiscoverableLogin(opts...)
	if err != nil {
		return nil, nil, fmt.Errorf("webauthnx: begin login: %w", err)
	}
	return assertion.Response, s.newPending(session, rpID), nil
}

// LoginResult is who signed in and whether the key verified the person.
type LoginResult struct {
	UserID string
	// UserVerified is true when the key checked a PIN or biometric, which
	// already counts as two factors.
	UserVerified bool
}

// CompleteLogin verifies the assertion against the stored key.
func (s *Service) CompleteLogin(ctx context.Context, r *http.Request, response json.RawMessage, pending *Pending) (LoginResult, error) {
	origin, rpID, err := s.relyingParty(r)
	if err != nil {
		return LoginResult{}, err
	}
	p, err := s.readPending(pending, rpID)
	if err != nil {
		return LoginResult{}, err
	}
	parsed, perr := protocol.ParseCredentialRequestResponseBytes(response)
	if perr != nil || strings.TrimSpace(parsed.ID) == "" {
		return LoginResult{}, fail(400, "MISSING_FIELDS", "Security key response is incomplete.")
	}
	stored, err := s.Store.FindByCredentialID(ctx, parsed.ID, rpID)
	if err != nil {
		return LoginResult{}, err
	}
	if stored == nil {
		return LoginResult{}, fail(401, "SECURITY_KEY_UNKNOWN", "That security key is not registered.")
	}
	if handle := parsed.Response.UserHandle; len(handle) > 0 && string(handle) != stored.UserID {
		return LoginResult{}, fail(401, "SECURITY_KEY_UNKNOWN", "That security key is not registered.")
	}

	credID, err1 := base64.RawURLEncoding.DecodeString(stored.CredentialID)
	pub, err2 := base64.RawURLEncoding.DecodeString(stored.PublicKey)
	if err1 != nil || err2 != nil {
		return LoginResult{}, fail(401, "SECURITY_KEY_REJECTED", "Security key could not be verified.")
	}
	wcred := webauthn.Credential{
		ID:        credID,
		PublicKey: pub,
		Flags: webauthn.CredentialFlags{
			UserPresent:    true,
			UserVerified:   stored.UserVerified,
			BackupEligible: stored.DeviceType == "multiDevice",
			BackupState:    stored.BackedUp,
		},
		Authenticator: webauthn.Authenticator{SignCount: stored.Counter},
	}
	for _, t := range stored.Transports {
		wcred.Transport = append(wcred.Transport, protocol.AuthenticatorTransport(t))
	}
	wa, err := s.client(origin, rpID)
	if err != nil {
		return LoginResult{}, err
	}
	// With a username the browser was offered all of that account's keys, and
	// the library requires the account to own every one of them, not only the
	// one that answered.
	owned := []webauthn.Credential{wcred}
	others, err := s.Store.ForUserAndRP(ctx, stored.UserID, rpID)
	if err != nil {
		return LoginResult{}, err
	}
	for _, d := range descriptors(others) {
		if !bytes.Equal(d.CredentialID, credID) {
			owned = append(owned, webauthn.Credential{ID: d.CredentialID})
		}
	}
	// Keys that are not discoverable return no user handle. It is not part of
	// the signature; the account is the one the credential is stored for.
	if len(parsed.Response.UserHandle) == 0 {
		parsed.Response.UserHandle = []byte(stored.UserID)
	}
	handler := func(rawID, userHandle []byte) (webauthn.User, error) {
		return user{id: stored.UserID, credentials: owned}, nil
	}
	verified, verr := wa.ValidateDiscoverableLogin(handler, p.Session, parsed)
	if verr != nil || verified == nil || verified.Authenticator.CloneWarning {
		// A sign counter that did not advance means the key may be cloned.
		return LoginResult{}, fail(401, "SECURITY_KEY_REJECTED", "Security key could not be verified.")
	}
	uv := parsed.Response.AuthenticatorData.Flags.HasUserVerified()
	if err := s.Store.UpdateAfterLogin(ctx, stored.RowID, verified.Authenticator.SignCount, uv); err != nil {
		return LoginResult{}, err
	}
	return LoginResult{UserID: stored.UserID, UserVerified: uv}, nil
}

// Rename changes a key's label.
func (s *Service) Rename(ctx context.Context, userID string, rowID int64, label string) ([]Payload, error) {
	next, err := normalizeLabel(label, "")
	if err != nil {
		return nil, err
	}
	if next == "" {
		return nil, fail(400, "MISSING_FIELDS", "Security key name is required.")
	}
	ok, err := s.Store.Rename(ctx, userID, rowID, next)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fail(404, "NOT_FOUND", "Security key not found.")
	}
	return s.List(ctx, userID)
}

// Delete removes a key.
func (s *Service) Delete(ctx context.Context, userID string, rowID int64) ([]Payload, error) {
	ok, err := s.Store.Delete(ctx, userID, rowID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fail(404, "NOT_FOUND", "Security key not found.")
	}
	return s.List(ctx, userID)
}
