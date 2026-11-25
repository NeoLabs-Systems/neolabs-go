package oauthlogin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/NeoLabs-Systems/neolabs-go/apperr"
	"github.com/NeoLabs-Systems/neolabs-go/dbx"
	"github.com/NeoLabs-Systems/neolabs-go/secretbox"
)

// ---- fakes ----------------------------------------------------------------

type fakeProvider struct {
	identity   Identity
	configured bool
	finishErr  error
	begun      struct{ state, verifier string }
	gotCode    string
	disabled   bool
	saved      []int64
}

func (f *fakeProvider) Key() string   { return "fake" }
func (f *fakeProvider) Label() string { return "Fake" }
func (f *fakeProvider) Icon() string  { return "" }
func (f *fakeProvider) Status(string) (bool, string) {
	if f.configured {
		return true, "ready"
	}
	return false, "Fake still needs administrator setup."
}
func (f *fakeProvider) Begin(_ context.Context, state, verifier, base string) (string, error) {
	f.begun.state, f.begun.verifier = state, verifier
	return "https://provider.example/auth?state=" + state + "&base=" + url.QueryEscape(base), nil
}
func (f *fakeProvider) Finish(_ context.Context, code, verifier string) (Identity, error) {
	f.gotCode = code
	if verifier != f.begun.verifier {
		return Identity{}, errors.New("verifier mismatch")
	}
	return f.identity, f.finishErr
}
func (f *fakeProvider) AssertEnabled() error {
	if f.disabled {
		return errors.New("Fake sign-in is disabled by the administrator.")
	}
	return nil
}
func (f *fakeProvider) SaveCredentials(_ context.Context, userID int64, _ Identity) error {
	f.saved = append(f.saved, userID)
	return nil
}

type fakeAccounts struct {
	db          *dbx.DB
	keyRequired bool
	usable      map[string]bool
}

func (a *fakeAccounts) SignupKeyRequired(context.Context) (bool, error) { return a.keyRequired, nil }
func (a *fakeAccounts) AssertSignupKeyUsable(_ context.Context, raw string) (string, error) {
	if !a.usable[raw] {
		return "", apperr.New(403, "That sign-up key isn’t valid.", "SIGNUP_KEY_INVALID")
	}
	return strings.ToUpper(raw), nil
}
func (a *fakeAccounts) ValidateNewUsername(ctx context.Context, name string) error {
	if len(name) < 3 {
		return apperr.New(400, "Username must be at least 3 characters.", "USERNAME_TOO_SHORT")
	}
	var one int
	if err := a.db.W.QueryRowContext(ctx, `SELECT 1 FROM users WHERE lower(username) = lower(?)`, name).Scan(&one); err == nil {
		return apperr.New(409, "Username is already taken", "USERNAME_TAKEN")
	}
	return nil
}
func (a *fakeAccounts) EmailTaken(ctx context.Context, email string) (bool, error) {
	var one int
	err := a.db.W.QueryRowContext(ctx, `SELECT 1 FROM users WHERE lower(email) = lower(?)`, email).Scan(&one)
	return err == nil, nil
}
func (a *fakeAccounts) CreateFromProvider(ctx context.Context, p Provider, id Identity, signupKey, username string) (int64, bool, error) {
	if username == "" {
		username = UsernameCandidate(id.Username)
		if username == "" {
			username = UsernameCandidate(id.DisplayName)
		}
		if username == "" {
			username = UsernameCandidate(strings.Split(id.Email, "@")[0])
		}
	}
	tx, err := a.db.W.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `INSERT INTO users (username, email, password_login_enabled) VALUES (?, ?, 0)`, username, nilIfEmpty(id.Email))
	if err != nil {
		return 0, false, apperr.New(409, "Username is already taken", "USERNAME_TAKEN")
	}
	uid, _ := res.LastInsertId()
	if err := SaveLink(ctx, tx, uid, p.Key(), id); err != nil {
		return 0, false, err
	}
	return uid, true, tx.Commit()
}

func newEngine(t *testing.T, prov *fakeProvider) (*Engine, *fakeAccounts) {
	t.Helper()
	db, err := dbx.Open(context.Background(), filepath.Join(t.TempDir(), "o.db"), dbx.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	for _, q := range []string{
		`CREATE TABLE users (id INTEGER PRIMARY KEY AUTOINCREMENT, username TEXT UNIQUE NOT NULL, email TEXT, password_login_enabled INTEGER DEFAULT 1)`,
		`CREATE TABLE auth_oauth_states (id INTEGER PRIMARY KEY AUTOINCREMENT, user_id INTEGER, provider_key TEXT NOT NULL, mode TEXT NOT NULL,
			state TEXT NOT NULL UNIQUE, code_verifier TEXT NOT NULL, expires_at TEXT NOT NULL, status TEXT DEFAULT 'pending', result_json TEXT,
			error_message TEXT, created_at TEXT DEFAULT (datetime('now')), completed_at TEXT, signup_key TEXT)`,
		`CREATE TABLE user_auth_providers (id INTEGER PRIMARY KEY AUTOINCREMENT, user_id INTEGER NOT NULL, provider_key TEXT NOT NULL,
			provider_user_id TEXT NOT NULL, email TEXT, metadata_json TEXT DEFAULT '{}', last_used_at TEXT,
			created_at TEXT DEFAULT (datetime('now')), updated_at TEXT DEFAULT (datetime('now')), UNIQUE(provider_key, provider_user_id))`,
	} {
		if _, err := db.W.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	box, _ := secretbox.New("enc:v1:", "test-key")
	acc := &fakeAccounts{db: db, usable: map[string]bool{}}
	return &Engine{DB: db, Box: box, Providers: []Provider{prov}, Accounts: acc, Product: "NeoAgent"}, acc
}

func addUser(t *testing.T, e *Engine, name, email string, pw int) int64 {
	t.Helper()
	res, err := e.DB.W.Exec(`INSERT INTO users (username, email, password_login_enabled) VALUES (?, ?, ?)`, name, email, pw)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}

func status(err error) (int, string) {
	if e, ok := apperr.As(err); ok {
		return e.Status, e.Code
	}
	return -1, ""
}

func begin(t *testing.T, e *Engine, mode string, userID int64) *Started {
	t.Helper()
	s, err := e.Begin(context.Background(), BeginParams{ProviderKey: "fake", Mode: mode, UserID: userID, BaseURL: "https://agent.example"})
	if err != nil {
		t.Fatalf("begin %s: %v", mode, err)
	}
	return s
}

// ---- tests ------------------------------------------------------------------

func TestBeginValidationAndStateStorage(t *testing.T) {
	prov := &fakeProvider{configured: true}
	e, acc := newEngine(t, prov)
	ctx := context.Background()

	for _, p := range []BeginParams{
		{ProviderKey: "nope", Mode: "login"},
		{ProviderKey: "fake", Mode: "weird"},
		{ProviderKey: "fake", Mode: "link"},
	} {
		if _, err := e.Begin(ctx, p); err == nil {
			t.Errorf("%+v must be refused", p)
		}
	}
	prov.configured = false
	if _, err := e.Begin(ctx, BeginParams{ProviderKey: "fake", Mode: "login"}); err == nil || err.Error() != "Fake still needs administrator setup." {
		t.Fatalf("unconfigured: %v", err)
	}
	prov.configured = true

	// Register needs a sign-up key when the server asks for one.
	acc.keyRequired = true
	if _, err := e.Begin(ctx, BeginParams{ProviderKey: "fake", Mode: "register"}); err == nil {
		t.Fatal("register without a key")
	} else if s, c := status(err); s != 403 || c != "SIGNUP_KEY_INVALID" {
		t.Fatalf("%v", err)
	}
	acc.usable["abc"] = true
	started, err := e.Begin(ctx, BeginParams{ProviderKey: "fake", Mode: "register", SignupKey: "abc", BaseURL: "https://agent.example"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(started.State, "auth_") || len(started.State) != 5+48 || started.Status != "oauth_redirect" || started.Mode != "register" {
		t.Fatalf("%+v", started)
	}
	if !strings.Contains(started.URL, started.State) {
		t.Fatal("the provider URL carries the state")
	}

	var verifier, key string
	e.DB.W.QueryRow(`SELECT code_verifier, signup_key FROM auth_oauth_states WHERE state = ?`, started.State).Scan(&verifier, &key)
	if !strings.HasPrefix(verifier, "enc:v1:") || strings.Contains(verifier, prov.begun.verifier) {
		t.Fatalf("the PKCE verifier is stored sealed: %q", verifier)
	}
	if key != "ABC" {
		t.Fatalf("the normalized key is kept for the callback: %q", key)
	}
	if len(prov.begun.verifier) < 60 {
		t.Fatalf("verifier too short: %d", len(prov.begun.verifier))
	}
}

func TestLoginFlow(t *testing.T) {
	prov := &fakeProvider{configured: true, identity: Identity{ProviderUserID: "g-1", Email: "neo@example.com", EmailVerified: true, DisplayName: "Neo"}}
	e, _ := newEngine(t, prov)
	ctx := context.Background()
	uid := addUser(t, e, "neo", "neo@example.com", 1)

	// Not linked yet: refused with guidance.
	s := begin(t, e, "login", 0)
	if _, err := e.Finish(ctx, s.State, "code"); err == nil || !strings.Contains(err.Error(), "No NeoAgent account is linked to this Fake account") {
		t.Fatalf("%v", err)
	}
	if c, err := e.Consume(ctx, s.State); err == nil || !strings.Contains(err.Error(), "No NeoAgent account") || c != nil {
		t.Fatalf("a failed flow reports its error to the client: %v", err)
	}
	if _, err := e.Consume(ctx, s.State); err == nil {
		t.Fatal("a consumed flow is gone")
	} else if st, _ := status(err); st != 404 {
		t.Fatalf("%v", err)
	}

	// Link from a signed-in session, then sign in with it.
	s = begin(t, e, "link", uid)
	res, err := e.Finish(ctx, s.State, "code-1")
	if err != nil || res.Action != "link" || res.UserID != uid || prov.gotCode != "code-1" {
		t.Fatalf("%+v %v", res, err)
	}
	if !equalIDs(prov.saved, uid) {
		t.Fatalf("credentials saved for %v", prov.saved)
	}
	done, err := e.Consume(ctx, s.State)
	if err != nil || done.Status != "completed" || done.Mode != "link" || done.Result["action"] != "link" {
		t.Fatalf("%+v %v", done, err)
	}

	s = begin(t, e, "login", 0)
	res, err = e.Finish(ctx, s.State, "code-2")
	if err != nil || res.Action != "login" || res.UserID != uid || res.Email != "neo@example.com" || res.ProviderLabel != "Fake" {
		t.Fatalf("%+v %v", res, err)
	}
	// A state is spent after one finish.
	if _, err := e.Finish(ctx, s.State, "again"); err == nil {
		t.Fatal("second finish of the same state")
	}
}

func equalIDs(got []int64, want ...int64) bool {
	if len(got) < len(want) {
		return false
	}
	return got[len(got)-1] == want[len(want)-1]
}

func TestLinkConflictAndUnlinkRules(t *testing.T) {
	prov := &fakeProvider{configured: true, identity: Identity{ProviderUserID: "g-1", Email: "x@example.com"}}
	e, _ := newEngine(t, prov)
	ctx := context.Background()
	a := addUser(t, e, "alice", "a@example.com", 0) // no password: the link is the only way in
	b := addUser(t, e, "bob", "b@example.com", 1)

	s := begin(t, e, "link", a)
	if _, err := e.Finish(ctx, s.State, "c"); err != nil {
		t.Fatal(err)
	}
	s = begin(t, e, "link", b)
	if _, err := e.Finish(ctx, s.State, "c"); err == nil || !strings.Contains(err.Error(), "already linked to another NeoAgent account") {
		t.Fatalf("%v", err)
	}

	list, err := e.ListForUser(ctx, a)
	if err != nil || len(list) != 1 || list[0].Label != "Fake" || list[0].Icon != "fake" || list[0].CanUnlink {
		t.Fatalf("%+v %v", list, err)
	}
	if removed, err := e.Unlink(ctx, a, list[0].ID); err == nil || removed {
		t.Fatal("the last sign-in method cannot be removed")
	}
	if removed, err := e.Unlink(ctx, b, list[0].ID); err != nil || removed {
		t.Fatalf("another account's link is not removable: %v %v", removed, err)
	}
	if _, err := e.Unlink(ctx, a, 0); err == nil {
		t.Fatal("invalid id")
	}
	e.DB.W.Exec(`UPDATE users SET password_login_enabled = 1 WHERE id = ?`, a)
	list, _ = e.ListForUser(ctx, a)
	if !list[0].CanUnlink {
		t.Fatal("with a password the link can go")
	}
	if removed, err := e.Unlink(ctx, a, list[0].ID); err != nil || !removed {
		t.Fatalf("%v %v", removed, err)
	}
}

func TestRegisterCreatesAccountOrAsksForUsername(t *testing.T) {
	prov := &fakeProvider{configured: true, identity: Identity{ProviderUserID: "g-9", Email: "new.person@example.com", EmailVerified: true, DisplayName: "New Person"}}
	e, _ := newEngine(t, prov)
	ctx := context.Background()

	s := begin(t, e, "register", 0)
	res, err := e.Finish(ctx, s.State, "c")
	if err != nil || res.Action != "register" || res.Created == nil || !*res.Created || res.UserID == 0 {
		t.Fatalf("%+v %v", res, err)
	}
	var name string
	e.DB.W.QueryRow(`SELECT username FROM users WHERE id = ?`, res.UserID).Scan(&name)
	if name != "newperson" {
		t.Fatalf("username derived from the display name: %q", name)
	}
	// Signing up again with the same identity signs the existing account in.
	s = begin(t, e, "register", 0)
	if res, err = e.Finish(ctx, s.State, "c"); err != nil || *res.Created {
		t.Fatalf("%+v %v", res, err)
	}

	// An email that belongs to someone else is refused.
	other := &fakeProvider{configured: true, identity: Identity{ProviderUserID: "g-10", Email: "NEW.PERSON@example.com"}}
	e.Providers = []Provider{other}
	s2, _ := e.Begin(ctx, BeginParams{ProviderKey: "fake", Mode: "register", BaseURL: "https://x"})
	if _, err := e.Finish(ctx, s2.State, "c"); err == nil || !strings.Contains(err.Error(), "already belongs to an existing account") {
		t.Fatalf("%v", err)
	}
}

func TestRegisterNeedingAUsername(t *testing.T) {
	prov := &fakeProvider{configured: true, identity: Identity{ProviderUserID: "or-1"}} // no name, no email
	e, _ := newEngine(t, prov)
	ctx := context.Background()
	s := begin(t, e, "register", 0)
	res, err := e.Finish(ctx, s.State, "c")
	if err != nil || !res.RequiresUsername {
		t.Fatalf("%+v %v", res, err)
	}
	var held string
	e.DB.W.QueryRow(`SELECT result_json FROM auth_oauth_states WHERE state = ?`, s.State).Scan(&held)
	if !strings.HasPrefix(held, "enc:v1:") || strings.Contains(held, "or-1") {
		t.Fatalf("the held identity is stored sealed: %q", held)
	}
	c, err := e.Consume(ctx, s.State)
	if err != nil || !c.RequiresUsername || c.Status != "requires_username" {
		t.Fatalf("%+v %v", c, err)
	}
	if _, err := e.Consume(ctx, s.State); err != nil {
		t.Fatal("a flow waiting for a username survives being read")
	}

	if err := e.CompleteRegistration(ctx, s.State, "ab"); err == nil {
		t.Fatal("too short")
	}
	addUser(t, e, "taken", "t@example.com", 1)
	if err := e.CompleteRegistration(ctx, s.State, "taken"); err == nil {
		t.Fatal("taken")
	}
	if err := e.CompleteRegistration(ctx, s.State, "fresh-name"); err != nil {
		t.Fatal(err)
	}
	done, err := e.Consume(ctx, s.State)
	if err != nil || done.Status != "completed" || done.Result["action"] != "register" || done.Result["created"] != true {
		t.Fatalf("%+v %v", done, err)
	}
	if err := e.CompleteRegistration(ctx, "auth_unknown", "whoever"); err == nil {
		t.Fatal("unknown state")
	} else if st, _ := status(err); st != 404 {
		t.Fatal(err)
	}
}

func TestFailPendingAndExpiry(t *testing.T) {
	prov := &fakeProvider{configured: true}
	e, _ := newEngine(t, prov)
	ctx := context.Background()
	s := begin(t, e, "login", 0)
	if c, _ := e.Consume(ctx, s.State); c.Status != "pending" {
		t.Fatalf("%+v", c)
	}
	d, err := e.Describe(ctx, s.State)
	if err != nil || d == nil || d.ProviderLabel != "Fake" || d.Mode != "login" {
		t.Fatalf("%+v %v", d, err)
	}
	if err := e.Fail(ctx, s.State, "access_denied"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Consume(ctx, s.State); err == nil || err.Error() != "access_denied" {
		t.Fatalf("%v", err)
	}
	if d, _ := e.Describe(ctx, s.State); d != nil {
		t.Fatal("a failed flow is no longer describable")
	}

	s = begin(t, e, "login", 0)
	e.DB.W.Exec(`UPDATE auth_oauth_states SET expires_at = ? WHERE state = ?`, isoNow(time.Now().Add(-time.Hour)), s.State)
	if _, err := e.Finish(ctx, s.State, "c"); err == nil || err.Error() != "OAuth state is missing or expired." {
		t.Fatalf("%v", err)
	}
}

func TestDisabledProviderBlocksTheOutcome(t *testing.T) {
	prov := &fakeProvider{configured: true, identity: Identity{ProviderUserID: "g-1", Email: "a@example.com"}}
	e, _ := newEngine(t, prov)
	ctx := context.Background()
	uid := addUser(t, e, "neo", "neo@example.com", 1)
	s := begin(t, e, "link", uid)
	e.Finish(ctx, s.State, "c")
	prov.disabled = true
	if _, err := e.Consume(ctx, s.State); err == nil || !strings.Contains(err.Error(), "disabled by the administrator") {
		t.Fatalf("%v", err)
	}
}

func TestListShowsStatus(t *testing.T) {
	prov := &fakeProvider{}
	e, _ := newEngine(t, prov)
	list := e.List(context.Background(), "https://x")
	if len(list) != 1 || list[0].ID != "fake" || list[0].Icon != "fake" || list[0].Configured || list[0].Summary == "" {
		t.Fatalf("%+v", list)
	}
}

func TestUsernameCandidates(t *testing.T) {
	for in, want := range map[string]string{
		"Neo Anderson": "neoanderson", "  __x__ ": "", "ab": "", "a.b-c_d": "a.b-c_d", "..neo..": "neo",
		"Ünï Cödé 99": "ncd99", strings.Repeat("a", 40): strings.Repeat("a", 32), "": "",
	} {
		if got := UsernameCandidate(in); got != want {
			t.Errorf("%q: got %q want %q", in, got, want)
		}
	}
	if HasUsernameCandidate(Identity{}) || !HasUsernameCandidate(Identity{Email: "someone@x.io"}) || HasUsernameCandidate(Identity{Email: "a@x.io"}) {
		t.Fatal("HasUsernameCandidate")
	}
}

// ---- Google -------------------------------------------------------------------

func TestGoogleAuthURLMatchesNode(t *testing.T) {
	raw, err := os.ReadFile("testdata/google_auth_url.json")
	if err != nil {
		t.Fatal(err)
	}
	var want struct {
		Origin string
		Params map[string]string
	}
	json.Unmarshal(raw, &want)

	g := Google{Cfg: GoogleConfig{ClientID: "cid.apps.example", ClientSecret: "csecret", RedirectURI: "https://agent.example.com/api/integrations/oauth/callback"}}
	// The Node vector used the literal challenge "CHALLENGE123"; build the same URL
	// from a verifier whose S256 challenge we substitute.
	u, err := g.Begin(context.Background(), "auth_abc", "verifier", "")
	if err != nil {
		t.Fatal(err)
	}
	parsed, _ := url.Parse(u)
	if parsed.Scheme+"://"+parsed.Host+parsed.Path != want.Origin {
		t.Fatalf("origin %s", parsed.Host+parsed.Path)
	}
	got := map[string]string{}
	for k, v := range parsed.Query() {
		got[k] = v[0]
	}
	if got["code_challenge"] != CodeChallenge("verifier") {
		t.Fatal("PKCE challenge is S256 of the verifier")
	}
	got["code_challenge"] = want.Params["code_challenge"]
	if len(got) != len(want.Params) {
		t.Fatalf("param sets differ: %v vs %v", got, want.Params)
	}
	for k, v := range want.Params {
		if got[k] != v {
			t.Errorf("%s: got %q want %q", k, got[k], v)
		}
	}
	if CodeChallenge("dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk") != "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM" {
		t.Fatal("RFC 7636 appendix B vector")
	}
}

func TestGoogleFinish(t *testing.T) {
	var gotForm url.Values
	var gotAuth string
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		gotForm = r.PostForm
		json.NewEncoder(w).Encode(map[string]any{"access_token": "at-1"})
	})
	mux.HandleFunc("/userinfo", func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		json.NewEncoder(w).Encode(map[string]any{"id": "123", "email": "Neo@Example.COM", "verified_email": true, "name": " Neo ",
			"picture": "https://img/x.png", "given_name": "Neo", "family_name": "", "locale": "en"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	g := Google{Cfg: GoogleConfig{ClientID: "cid", ClientSecret: "sec", RedirectURI: "https://r", TokenURL: srv.URL + "/token", UserInfoURL: srv.URL + "/userinfo"}}

	id, err := g.Finish(context.Background(), "the-code", "the-verifier")
	if err != nil {
		t.Fatal(err)
	}
	if gotForm.Get("code") != "the-code" || gotForm.Get("code_verifier") != "the-verifier" || gotForm.Get("grant_type") != "authorization_code" ||
		gotForm.Get("client_secret") != "sec" || gotForm.Get("redirect_uri") != "https://r" || gotAuth != "Bearer at-1" {
		t.Fatalf("%v %s", gotForm, gotAuth)
	}
	if id.ProviderUserID != "123" || id.Email != "neo@example.com" || !id.EmailVerified || id.DisplayName != "Neo" ||
		id.AvatarURL == nil || *id.AvatarURL != "https://img/x.png" || id.Metadata["givenName"] != "Neo" || id.Metadata["familyName"] != nil {
		t.Fatalf("%+v", id)
	}

	// A profile without an email is not a usable identity.
	mux.HandleFunc("/noemail", func(w http.ResponseWriter, r *http.Request) { json.NewEncoder(w).Encode(map[string]any{"id": "1"}) })
	g.Cfg.UserInfoURL = srv.URL + "/noemail"
	if _, err := g.Finish(context.Background(), "c", "v"); err == nil || err.Error() != "Google did not return a usable account identity." {
		t.Fatalf("%v", err)
	}
	g.Cfg.TokenURL = srv.URL + "/missing"
	if _, err := g.Finish(context.Background(), "c", "v"); err == nil {
		t.Fatal("a rejected code is an error")
	}
	if ok, _ := (Google{}).Status(""); ok {
		t.Fatal("unconfigured")
	}
	if ok, s := g.Status(""); !ok || s != "Google sign-in is ready for account connections." {
		t.Fatal(s)
	}
}
