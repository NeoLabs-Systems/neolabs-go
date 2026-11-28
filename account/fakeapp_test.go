package account

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NeoLabs-Systems/neolabs-go/dbx"
	"github.com/NeoLabs-Systems/neolabs-go/lockout"
	"github.com/NeoLabs-Systems/neolabs-go/passhash"
	"github.com/NeoLabs-Systems/neolabs-go/servicemail"
	"github.com/NeoLabs-Systems/neolabs-go/sessionx"
	"github.com/NeoLabs-Systems/neolabs-go/userid"
)

// fakeApp is a small app built the way the NeoLabs apps are: a SQLite users
// table (numbered or UUID ids), notes with files on disk, a sessionx store.
type fakeApp struct {
	t        *testing.T
	db       *dbx.DB
	uuid     bool
	dir      string // the app's file root
	sessions *sessionx.Store
	mail     *fakeMailer
	rec      *Recovery
	h        *Handlers
	mux      *http.ServeMux
	events   chan Event
	n        int
}

type sent struct {
	To  string
	Msg servicemail.Message
}

type fakeMailer struct {
	mu   sync.Mutex
	sent []sent
	fail bool
}

func (m *fakeMailer) Send(_ context.Context, to string, msg servicemail.Message) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail {
		return errors.New("smtp down")
	}
	m.sent = append(m.sent, sent{to, msg})
	return nil
}

func (m *fakeMailer) all() []sent {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]sent(nil), m.sent...)
}

func (m *fakeMailer) last(t *testing.T) sent {
	t.Helper()
	all := m.all()
	if len(all) == 0 {
		t.Fatal("no mail sent")
	}
	return all[len(all)-1]
}

func tokenOf(t *testing.T, link string) string {
	t.Helper()
	u, err := url.Parse(link)
	if err != nil || u.Query().Get("token") == "" {
		t.Fatalf("link %q carries no token", link)
	}
	return u.Query().Get("token")
}

const goodCode = "123456"

func newFakeApp(t *testing.T, uuid bool) *fakeApp {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	db, err := dbx.Open(ctx, filepath.Join(dir, "app.db"), dbx.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	idType := "INTEGER PRIMARY KEY AUTOINCREMENT"
	ref := "INTEGER"
	if uuid {
		idType, ref = "TEXT PRIMARY KEY", "TEXT"
	}
	for _, q := range []string{
		`CREATE TABLE users (id ` + idType + `, username TEXT NOT NULL UNIQUE COLLATE NOCASE, email TEXT UNIQUE COLLATE NOCASE,
			email_verified_at TEXT, password TEXT NOT NULL, is_admin INTEGER NOT NULL DEFAULT 0, disabled_at TEXT,
			totp INTEGER NOT NULL DEFAULT 0, api_secret TEXT)`,
		`CREATE TABLE user_email_tokens (id INTEGER PRIMARY KEY AUTOINCREMENT, user_id ` + ref + ` NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			type TEXT NOT NULL, email TEXT NOT NULL, token_hash TEXT NOT NULL UNIQUE, created_at TEXT DEFAULT (datetime('now')),
			expires_at TEXT NOT NULL, consumed_at TEXT)`,
		`CREATE TABLE login_failures (account_key TEXT NOT NULL, kind TEXT NOT NULL, failed_at_ms INTEGER NOT NULL)`,
		`CREATE TABLE notes (id INTEGER PRIMARY KEY, user_id ` + ref + ` NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			body TEXT, file TEXT, token_hash TEXT, picture BLOB)`,
	} {
		if _, err := db.W.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	store, err := sessionx.NewStore(ctx, db.W)
	if err != nil {
		t.Fatal(err)
	}
	a := &fakeApp{t: t, db: db, uuid: uuid, dir: filepath.Join(dir, "files"), sessions: store, mail: &fakeMailer{}, events: make(chan Event, 16)}
	a.rec = &Recovery{
		DB: db.W, Directory: a, Mailer: a.mail,
		PublicURL: "https://app.example.com/", ResetPath: "/api/auth/password/reset", ConfirmPath: "/api/auth/email/confirm",
		RequireChangeConfirmation: true,
		Messages: Messages{
			Reset: func(acc Account, link string) servicemail.Message {
				return servicemail.Message{Subject: "reset", ActionURL: link}
			},
			Signup: func(acc Account, link string) servicemail.Message {
				return servicemail.Message{Subject: "signup", ActionURL: link}
			},
			Change: func(acc Account, to, link string) servicemail.Message {
				return servicemail.Message{Subject: "change", ActionURL: link}
			},
			ChangeRequested: func(acc Account, to string) servicemail.Message {
				return servicemail.Message{Subject: "change requested " + to}
			},
		},
		ValidatePassword: func(p string) error {
			if len(p) < 12 {
				return errors.New("Use at least 12 characters.")
			}
			return nil
		},
		SetPassword: func(ctx context.Context, tx *sql.Tx, id userid.ID, password string) error {
			hash, err := passhash.Hash(password, 4)
			if err != nil {
				return err
			}
			_, err = tx.ExecContext(ctx, `UPDATE users SET password = ? WHERE id = ?`, hash, string(id))
			return err
		},
		Sessions: store,
		Changed:  func(_ context.Context, e Event) { a.events <- e },
	}
	a.h = &Handlers{
		Directory: a,
		Identify: func(r *http.Request) (userid.ID, bool) {
			v := r.Header.Get("X-User")
			return userid.ID(v), v != ""
		},
		CurrentSession: func(r *http.Request) string { return r.Header.Get("X-Session") },
		Reauth:         &Reauth{Lockout: lockout.Store{DB: db.W}, Verifier: a},
		Recovery:       a.rec,
		Pages:          AuthPages{Brand: "Fake", ResetPath: "/api/auth/password/reset", ConfirmPath: "/api/auth/email/confirm"},
		MinLatency:     40 * time.Millisecond,
		AnonymousMessage: func(_ *http.Request, kind string) string {
			return "If that account exists, Fake will send an email (" + kind + ")."
		},
		Sessions: StoreSessions{Store: store},
	}
	a.mux = http.NewServeMux()
	a.mux.Handle("POST /api/auth/password/forgot", a.h.Forgot())
	a.mux.Handle("GET /api/auth/password/reset", a.h.ResetPage())
	a.mux.Handle("POST /api/auth/password/reset", a.h.ResetSubmit())
	a.mux.Handle("GET /api/auth/email/confirm", a.h.ConfirmPage())
	a.mux.Handle("POST /api/auth/email/confirm", a.h.ConfirmSubmit())
	a.mux.Handle("POST /api/auth/email/resend", a.h.ResendSignup())
	a.mux.Handle("PUT /api/account/email", a.h.ChangeEmail(nil))
	a.mux.Handle("POST /api/account/email/resend", a.h.ResendEmail(nil))
	a.mux.Handle("GET /api/account/sessions", a.h.ListSessions())
	a.mux.Handle("DELETE /api/account/sessions/{id}", a.h.RevokeSession("id"))
	a.mux.Handle("POST /api/account/sessions/revoke-all", a.h.SignOutEverywhere(true, nil))
	a.mux.Handle("POST /api/auth/logout-all", a.h.SignOutEverywhere(false, nil))
	return a
}

// addUser creates an account with password "correct horse battery".
func (a *fakeApp) addUser(name string, admin, totp bool) userid.ID {
	a.t.Helper()
	hash, _ := passhash.Hash("correct horse battery", 4)
	a.n++
	var id userid.ID
	if a.uuid {
		id = userid.ID(fmt.Sprintf("0000000%d-aaaa-4bbb-8ccc-dddddddddddd", a.n))
		_, err := a.db.W.Exec(`INSERT INTO users (id, username, email, password, is_admin, totp) VALUES (?, ?, ?, ?, ?, ?)`,
			string(id), name, name+"@example.com", hash, admin, totp)
		if err != nil {
			a.t.Fatal(err)
		}
		return id
	}
	res, err := a.db.W.Exec(`INSERT INTO users (username, email, password, is_admin, totp) VALUES (?, ?, ?, ?, ?)`,
		name, name+"@example.com", hash, admin, totp)
	if err != nil {
		a.t.Fatal(err)
	}
	n, _ := res.LastInsertId()
	return userid.FromInt(n)
}

func (a *fakeApp) scanAccount(row *sql.Row) (Account, error) {
	var acc Account
	var email, verified, disabled sql.NullString
	err := row.Scan(&acc.ID, &acc.Username, &email, &verified, &acc.Admin, &disabled)
	if errors.Is(err, sql.ErrNoRows) {
		return Account{}, ErrNotFound
	}
	acc.Email, acc.EmailVerified, acc.Disabled = email.String, verified.Valid, disabled.Valid
	return acc, err
}

const accountCols = `SELECT id, username, email, email_verified_at, is_admin = 1 AND disabled_at IS NULL, disabled_at FROM users`

func (a *fakeApp) Account(ctx context.Context, id userid.ID) (Account, error) {
	return a.scanAccount(a.db.R.QueryRowContext(ctx, accountCols+` WHERE id = ?`, string(id)))
}

func (a *fakeApp) Lookup(ctx context.Context, login string) (Account, error) {
	return a.scanAccount(a.db.R.QueryRowContext(ctx, accountCols+` WHERE lower(username) = lower(?1) OR lower(email) = lower(?1)`, login))
}

func (a *fakeApp) Factors(ctx context.Context, id userid.ID) (Factors, error) {
	var totp bool
	err := a.db.R.QueryRowContext(ctx, `SELECT totp FROM users WHERE id = ?`, string(id)).Scan(&totp)
	return Factors{Password: true, TwoFactor: totp}, err
}

func (a *fakeApp) CheckPassword(ctx context.Context, id userid.ID, password string) (bool, error) {
	var hash string
	if err := a.db.R.QueryRowContext(ctx, `SELECT password FROM users WHERE id = ?`, string(id)).Scan(&hash); err != nil {
		return false, err
	}
	return passhash.Verify(hash, password), nil
}

func (a *fakeApp) CheckCode(_ context.Context, _ userid.ID, code string) (bool, error) {
	return code == goodCode, nil
}

func (a *fakeApp) login(id userid.ID) string {
	a.t.Helper()
	token, _, err := a.sessions.IssueBearer(context.Background(), "tok_", map[string]any{
		"userId": string(id), "userAgent": "test", "createdAt": time.Now().UTC().Format(time.RFC3339)}, time.Hour)
	if err != nil {
		a.t.Fatal(err)
	}
	return passhash.DigestToken(token)
}

type reply struct {
	Code   int
	Header http.Header
	Body   string
}

func (r reply) json(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(r.Body), &m); err != nil {
		t.Fatalf("%d not JSON: %s", r.Code, r.Body)
	}
	return m
}

// do sends a request as user (empty for anonymous) on session sid.
func (a *fakeApp) do(method, target string, user userid.ID, sid string, body any) reply {
	a.t.Helper()
	return serve(a.mux, method, target, user, sid, body)
}

func serve(h http.Handler, method, target string, user userid.ID, sid string, body any) reply {
	var rd io.Reader
	ct := ""
	switch b := body.(type) {
	case nil:
	case url.Values:
		rd, ct = strings.NewReader(b.Encode()), "application/x-www-form-urlencoded"
	default:
		raw, _ := json.Marshal(b)
		rd, ct = strings.NewReader(string(raw)), "application/json"
	}
	req := httptest.NewRequest(method, target, rd)
	req.Host = "attacker.example"
	if ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	if user != "" {
		req.Header.Set("X-User", string(user))
	}
	if sid != "" {
		req.Header.Set("X-Session", sid)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return reply{rec.Code, rec.Header(), rec.Body.String()}
}

func (a *fakeApp) count(query string, args ...any) int {
	a.t.Helper()
	var n int
	if err := a.db.W.QueryRow(query, args...).Scan(&n); err != nil {
		a.t.Fatal(err)
	}
	return n
}

func (a *fakeApp) event(t *testing.T) Event {
	t.Helper()
	select {
	case e := <-a.events:
		return e
	case <-time.After(2 * time.Second):
		t.Fatal("no change event")
		return Event{}
	}
}

// each runs a test against the numbered and the UUID app.
func each(t *testing.T, test func(t *testing.T, a *fakeApp)) {
	for _, uuid := range []bool{false, true} {
		name := "numbered"
		if uuid {
			name = "uuid"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			test(t, newFakeApp(t, uuid))
		})
	}
}
