package account

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NeoLabs-Systems/neolabs-go/lockout"
	"github.com/NeoLabs-Systems/neolabs-go/userid"
)

func TestForgotAnswersTheSameAndMailsOnlyRealAccounts(t *testing.T) {
	each(t, func(t *testing.T, a *fakeApp) {
		a.addUser("neo", false, false)
		start := time.Now()
		known := a.do("POST", "/api/auth/password/forgot", "", "", map[string]any{"account": "NEO@example.com"})
		if time.Since(start) < 40*time.Millisecond {
			t.Fatal("the answer is not padded")
		}
		unknown := a.do("POST", "/api/auth/password/forgot", "", "", map[string]any{"account": "nobody"})
		if known.Code != 200 || known.Body != unknown.Body {
			t.Fatalf("answers differ:\n%d %s\n%d %s", known.Code, known.Body, unknown.Code, unknown.Body)
		}
		if m := known.json(t); m["success"] != true || !strings.Contains(m["message"].(string), "password_reset") {
			t.Fatalf("%v", m)
		}
		a.rec.Wait()
		mails := a.mail.all()
		if len(mails) != 1 || mails[0].To != "neo@example.com" {
			t.Fatalf("mails %+v", mails)
		}
		link := mails[0].Msg.ActionURL
		if !strings.HasPrefix(link, "https://app.example.com/api/auth/password/reset?token=") {
			t.Fatalf("link must come from the public URL, not the Host: %s", link)
		}
		if len(tokenOf(t, link)) != 43 {
			t.Fatal("tokens carry 256 bits")
		}

		for range 4 {
			a.do("POST", "/api/auth/password/forgot", "", "", map[string]any{"account": "neo"})
		}
		a.rec.Wait()
		if n := len(a.mail.all()); n != 3 {
			t.Fatalf("one account gets at most 3 reset mails per window, got %d", n)
		}
	})
}

func TestResetSpendsTheLinkOnceAndOnlyByPost(t *testing.T) {
	each(t, func(t *testing.T, a *fakeApp) {
		id := a.addUser("neo", false, false)
		sid := a.login(id)
		if err := a.rec.RequestReset(context.Background(), "neo"); err != nil {
			t.Fatal(err)
		}
		token := tokenOf(t, a.mail.last(t).Msg.ActionURL)

		page := a.do("GET", "/api/auth/password/reset?token="+url.QueryEscape(token), "", "", nil)
		if page.Code != 200 || !strings.Contains(page.Body, `value="`+token+`"`) || page.Header.Get("Cache-Control") != "no-store" {
			t.Fatalf("%d %s", page.Code, page.Body)
		}
		post := func(pw, confirm string) reply {
			return a.do("POST", "/api/auth/password/reset", "", "", url.Values{"token": {token}, "password": {pw}, "confirmPassword": {confirm}})
		}
		if r := post("short", "short"); r.Code != 400 || !strings.Contains(r.Body, "12 characters") {
			t.Fatalf("weak password: %d %s", r.Code, r.Body)
		}
		if r := post("a new long password", "another"); r.Code != 400 || !strings.Contains(r.Body, "do not match") {
			t.Fatalf("mismatch: %d", r.Code)
		}
		if r := post("a new long password", "a new long password"); r.Code != 200 || !strings.Contains(r.Body, "Password changed") {
			t.Fatalf("a refused password must not spend the link: %d %s", r.Code, r.Body)
		}
		if ok, _ := a.CheckPassword(context.Background(), id, "a new long password"); !ok {
			t.Fatal("password not set")
		}
		if d, _ := a.sessions.Get(context.Background(), sid); d != nil {
			t.Fatal("sessions survive a reset")
		}
		if e := a.event(t); e.Kind != PasswordReset || e.Account.ID != id {
			t.Fatalf("event %+v", e)
		}
		if r := post("yet another password", ""); r.Code != 400 || !strings.Contains(r.Body, "invalid or expired") {
			t.Fatalf("a link works once: %d", r.Code)
		}
	})
}

func TestResetRacesSpendTheLinkOnce(t *testing.T) {
	a := newFakeApp(t, false)
	a.addUser("neo", false, false)
	a.rec.RequestReset(context.Background(), "neo")
	token := tokenOf(t, a.mail.last(t).Msg.ActionURL)
	var wg sync.WaitGroup
	results := make(chan error, 8)
	for i := range 8 {
		wg.Go(func() {
			results <- a.rec.Reset(context.Background(), token, "racing password "+string(rune('a'+i)), "")
		})
	}
	wg.Wait()
	close(results)
	ok := 0
	for err := range results {
		if err == nil {
			ok++
		} else if err != ErrResetLink {
			t.Fatalf("unexpected %v", err)
		}
	}
	if ok != 1 {
		t.Fatalf("%d resets succeeded with one link", ok)
	}
}

func TestResetLinkDiesWithTheAddressAndIsCappedPerToken(t *testing.T) {
	a := newFakeApp(t, false)
	id := a.addUser("neo", false, false)
	ctx := context.Background()
	a.rec.RequestReset(ctx, "neo")
	token := tokenOf(t, a.mail.last(t).Msg.ActionURL)
	a.db.W.Exec(`UPDATE users SET email = 'other@example.com' WHERE id = ?`, string(id))
	if err := a.rec.Reset(ctx, token, "a long new password", ""); err != ErrResetLink {
		t.Fatalf("a link to an old address must be dead: %v", err)
	}
	for range 12 {
		a.rec.Reset(ctx, "guess", "a long new password", "")
	}
	if err := a.rec.Reset(ctx, "guess", "a long new password", ""); err != ErrTooManyTries {
		t.Fatalf("12 wrong tries per token, then it is locked: %v", err)
	}
	if err := a.rec.Reset(ctx, "", "x", ""); err != ErrResetLink {
		t.Fatal(err)
	}
	if err := a.rec.Reset(ctx, "t", strings.Repeat("x", 2000), ""); err != ErrPasswordTooLong {
		t.Fatal(err)
	}
}

func TestEmailDisabledOrWithoutPublicURLSendsNothing(t *testing.T) {
	a := newFakeApp(t, false)
	a.addUser("neo", false, false)
	a.rec.PublicURL = "app.example.com"
	if a.rec.Enabled() {
		t.Fatal("a public URL without scheme cannot build links")
	}
	a.rec.RequestReset(context.Background(), "neo")
	a.rec.PublicURL = "https://app.example.com"
	a.rec.Mailer = nil
	a.rec.RequestReset(context.Background(), "neo")
	if len(a.mail.all()) != 0 || a.count(`SELECT COUNT(*) FROM user_email_tokens`) != 0 {
		t.Fatal("nothing may be issued without email")
	}
}

func TestSignupConfirmationIsSpentOnlyByPost(t *testing.T) {
	each(t, func(t *testing.T, a *fakeApp) {
		id := a.addUser("neo", false, false)
		acc, _ := a.Account(context.Background(), id)
		if err := a.rec.SendSignupConfirmation(context.Background(), acc); err != nil {
			t.Fatal(err)
		}
		token := tokenOf(t, a.mail.last(t).Msg.ActionURL)
		for range 2 {
			page := a.do("GET", "/api/auth/email/confirm?token="+url.QueryEscape(token), "", "", nil)
			if page.Code != 200 || !strings.Contains(page.Body, `method="post"`) {
				t.Fatalf("%d %s", page.Code, page.Body)
			}
		}
		if acc, _ := a.Account(context.Background(), id); acc.EmailVerified {
			t.Fatal("GET confirmed the address")
		}
		done := a.do("POST", "/api/auth/email/confirm", "", "", url.Values{"token": {token}})
		if done.Code != 200 || !strings.Contains(done.Body, "Email confirmed") {
			t.Fatalf("%d %s", done.Code, done.Body)
		}
		if acc, _ := a.Account(context.Background(), id); !acc.EmailVerified {
			t.Fatal("not confirmed")
		}
		if e := a.event(t); e.Kind != EmailConfirmed {
			t.Fatalf("%+v", e)
		}
		if again := a.do("POST", "/api/auth/email/confirm", "", "", url.Values{"token": {token}}); again.Code != 400 {
			t.Fatalf("spent twice: %d", again.Code)
		}
		if acc, _ := a.Account(context.Background(), id); a.rec.SendSignupConfirmation(context.Background(), acc) != ErrNothingToConfirm {
			t.Fatal("a confirmed address needs no link")
		}
	})
}

func TestResendSignupAnswersTheSame(t *testing.T) {
	a := newFakeApp(t, false)
	a.addUser("neo", false, false)
	a.addUser("done", false, false)
	a.db.W.Exec(`UPDATE users SET email_verified_at = datetime('now') WHERE username = 'done'`)
	r1 := a.do("POST", "/api/auth/email/resend", "", "", map[string]any{"account": "neo"})
	r2 := a.do("POST", "/api/auth/email/resend", "", "", map[string]any{"account": "done"})
	r3 := a.do("POST", "/api/auth/email/resend", "", "", map[string]any{"account": "ghost"})
	if r1.Body != r2.Body || r2.Body != r3.Body || r1.Code != 200 {
		t.Fatal("anonymous answers differ")
	}
	if r := a.do("POST", "/api/auth/email/resend", "", "", map[string]any{"account": strings.Repeat("x", 400)}); r.Code != 400 {
		t.Fatal("overlong login accepted")
	}
	a.rec.Wait()
	if m := a.mail.all(); len(m) != 1 || m[0].To != "neo@example.com" || m[0].Msg.Subject != "signup" {
		t.Fatalf("%+v", m)
	}
}

func TestEmailChangeNeedsReauthAndConfirmation(t *testing.T) {
	each(t, func(t *testing.T, a *fakeApp) {
		id := a.addUser("neo", false, true)
		a.addUser("taken", false, false)
		change := func(body map[string]any) reply { return a.do("PUT", "/api/account/email", id, "", body) }

		if r := change(map[string]any{"email": "new@example.com", "currentPassword": "nope", "code": goodCode}); r.Code != 401 || r.json(t)["code"] != "WRONG_PASSWORD" {
			t.Fatalf("%d %s", r.Code, r.Body)
		}
		if r := change(map[string]any{"email": "new@example.com", "currentPassword": "correct horse battery"}); r.json(t)["code"] != "TWO_FACTOR_REQUIRED" {
			t.Fatalf("%s", r.Body)
		}
		if r := change(map[string]any{"email": "not an address", "currentPassword": "correct horse battery", "code": goodCode}); r.json(t)["code"] != "INVALID_EMAIL" {
			t.Fatalf("%s", r.Body)
		}
		if r := change(map[string]any{"email": "TAKEN@example.com", "currentPassword": "correct horse battery", "code": goodCode}); r.Code != 409 {
			t.Fatalf("%d %s", r.Code, r.Body)
		}

		a.rec.RequestReset(context.Background(), "neo")
		oldReset := tokenOf(t, a.mail.last(t).Msg.ActionURL)

		r := change(map[string]any{"email": " New@Example.com ", "currentPassword": "correct horse battery", "code": goodCode})
		if r.Code != 200 || r.json(t)["pendingEmail"] != "new@example.com" {
			t.Fatalf("%d %s", r.Code, r.Body)
		}
		a.rec.Wait()
		mails := a.mail.all()
		var confirm, notice sent
		for _, m := range mails {
			switch m.Msg.Subject {
			case "change":
				confirm = m
			case "change requested new@example.com":
				notice = m
			}
		}
		if confirm.To != "new@example.com" || notice.To != "neo@example.com" {
			t.Fatalf("%+v", mails)
		}
		if p, _ := a.rec.PendingEmail(context.Background(), id); p != "new@example.com" {
			t.Fatal(p)
		}
		if acc, _ := a.Account(context.Background(), id); acc.Email != "neo@example.com" {
			t.Fatal("changed before confirmation")
		}
		resend := a.do("POST", "/api/account/email/resend", id, "", nil)
		if resend.Code != 200 || resend.json(t)["pendingEmail"] != "new@example.com" {
			t.Fatalf("%d %s", resend.Code, resend.Body)
		}
		token := tokenOf(t, a.mail.last(t).Msg.ActionURL)

		done := a.do("POST", "/api/auth/email/confirm", "", "", url.Values{"token": {token}})
		if done.Code != 200 || !strings.Contains(done.Body, "new@example.com") {
			t.Fatalf("%d %s", done.Code, done.Body)
		}
		acc, _ := a.Account(context.Background(), id)
		if acc.Email != "new@example.com" || !acc.EmailVerified {
			t.Fatalf("%+v", acc)
		}
		if e := a.event(t); e.Kind != EmailChanged || e.Account.Email != "neo@example.com" || e.Email != "new@example.com" {
			t.Fatalf("%+v", e)
		}
		if err := a.rec.Reset(context.Background(), oldReset, "a long new password", ""); err != ErrResetLink {
			t.Fatalf("a reset link sent to the old address survived: %v", err)
		}
	})
}

func TestEmailChangeWithoutConfirmationAndTakenAtConfirmTime(t *testing.T) {
	a := newFakeApp(t, false)
	id := a.addUser("neo", false, false)
	acc, _ := a.Account(context.Background(), id)
	a.rec.SendSignupConfirmation(context.Background(), acc)
	ctx := context.Background()

	pending, err := a.rec.ChangeEmail(ctx, acc, "late@example.com")
	if err != nil || pending == "" {
		t.Fatal(pending, err)
	}
	token := tokenOf(t, a.mail.last(t).Msg.ActionURL)
	a.addUser("late", false, false) // takes late@example.com meanwhile
	if _, err := a.rec.ConfirmEmail(ctx, token); err != ErrEmailTaken {
		t.Fatalf("%v", err)
	}

	a.rec.RequireChangeConfirmation = false
	if pending, err := a.rec.ChangeEmail(ctx, acc, "direct@example.com"); err != nil || pending != "" {
		t.Fatal(pending, err)
	}
	acc, _ = a.Account(ctx, id)
	if acc.Email != "direct@example.com" || acc.EmailVerified {
		t.Fatalf("a direct change stores the address unverified: %+v", acc)
	}
	if a.count(`SELECT COUNT(*) FROM user_email_tokens WHERE consumed_at IS NULL`) != 0 {
		t.Fatal("a new address retires every open link")
	}

	a.rec.Wait()
	a.mail.fail = true
	a.rec.RequireChangeConfirmation = true
	if _, err := a.rec.ChangeEmail(ctx, acc, "fails@example.com"); err != ErrSendFailed {
		t.Fatal(err)
	}
	if p, _ := a.rec.PendingEmail(ctx, id); p != "" {
		t.Fatal("a link that was not sent must not stay live")
	}
}

func TestReauthLocksOutPerAccount(t *testing.T) {
	a := newFakeApp(t, false)
	id := a.addUser("neo", false, false)
	ctx := context.Background()
	now := time.Now()
	a.h.Reauth.Lockout.Now = func() time.Time { return now }
	for i := range 10 {
		now = now.Add(time.Minute)
		if err := a.h.Reauth.Confirm(ctx, id, "wrong", "", false); err != ErrWrongPassword {
			t.Fatalf("try %d: %v", i, err)
		}
	}
	err := a.h.Reauth.Confirm(ctx, id, "correct horse battery", "", false)
	if p := Describe(err); p.Status != 429 || p.Code != "LOGIN_LOCKED" || p.RetryAfter <= 0 {
		t.Fatalf("%+v", p)
	}
	now = now.Add(20 * time.Minute)
	if err := a.h.Reauth.Confirm(ctx, id, "correct horse battery", "", false); err != nil {
		t.Fatal(err)
	}
	if err := a.h.Reauth.Confirm(ctx, id, "correct horse battery", "", true); err != ErrFactorRequired {
		t.Fatal(err)
	}
	a.db.W.Exec(`UPDATE users SET totp = 1`)
	if err := a.h.Reauth.Confirm(ctx, id, "correct horse battery", "000000", true); err != ErrWrongCode {
		t.Fatal(err)
	}
	if err := a.h.Reauth.Confirm(ctx, id, "correct horse battery", goodCode, true); err != nil {
		t.Fatal(err)
	}
	if a.count(`SELECT COUNT(*) FROM login_failures`) != 0 {
		t.Fatal("a success takes its failures back")
	}

}

type staticVerifier struct{ f Factors }

func (v staticVerifier) Factors(context.Context, userid.ID) (Factors, error) { return v.f, nil }
func (staticVerifier) CheckPassword(context.Context, userid.ID, string) (bool, error) {
	return false, nil
}
func (staticVerifier) CheckCode(context.Context, userid.ID, string) (bool, error) { return false, nil }

func TestPasswordlessAccountsConfirmWithTheirSession(t *testing.T) {
	r := Reauth{Verifier: staticVerifier{}}
	if err := r.Confirm(context.Background(), "1", "", "", false); err != nil {
		t.Fatal(err)
	}
	if err := r.Confirm(context.Background(), "1", "", "", true); err != ErrFactorRequired {
		t.Fatal(err)
	}
}

func TestWriteErrorEnvelope(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteError(rec, nil, &lockout.LockedError{RetryAfter: 7})
	if rec.Code != 429 || rec.Header().Get("Retry-After") != "7" ||
		rec.Body.String() != `{"code":"LOGIN_LOCKED","error":"Too many wrong passwords or codes for this account. Try again in 7 seconds.","retryAfter":7}` {
		t.Fatalf("%d %q", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	WriteError(rec, nil, errors.New("disk on fire at /var/lib/x"))
	if rec.Code != 500 || strings.Contains(rec.Body.String(), "disk") {
		t.Fatal(rec.Body.String())
	}
	rec = httptest.NewRecorder()
	WriteError(rec, nil, fmt.Errorf("wrapped: %w", ErrLastAdmin))
	if rec.Code != 409 || !strings.Contains(rec.Body.String(), `"code":"LAST_ADMIN"`) {
		t.Fatal(rec.Body.String())
	}
}
