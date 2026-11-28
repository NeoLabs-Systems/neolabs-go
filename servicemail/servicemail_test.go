package servicemail

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NeoLabs-Systems/neolabs-go/passhash"
	_ "modernc.org/sqlite"
)

type vectors struct {
	Cases []struct {
		Message struct {
			Subject, Title, Intro, ActionUrl, ActionLabel, Footer, BrandName string
			Details                                                          []struct{ Label, Value string }
		}
		HTML string
		Text string
	}
	TokenHash  string
	SqliteDate string
}

func load(t *testing.T) vectors {
	t.Helper()
	raw, err := os.ReadFile("testdata/vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var v vectors
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// Emails must render byte for byte like the Node server's service_email.js.
func TestRenderMatchesNode(t *testing.T) {
	for i, c := range load(t).Cases {
		m := Message{
			Subject: c.Message.Subject, Title: c.Message.Title, Intro: c.Message.Intro,
			ActionURL: c.Message.ActionUrl, ActionLabel: c.Message.ActionLabel,
			Footer: c.Message.Footer, BrandName: c.Message.BrandName,
		}
		for _, d := range c.Message.Details {
			m.Details = append(m.Details, Detail{d.Label, d.Value})
		}
		if got := RenderHTML(m); got != c.HTML {
			t.Errorf("case %d html differs (len got %d want %d)\n%s", i, len(got), len(c.HTML), firstDiff(got, c.HTML))
		}
		if got := RenderText(m); got != c.Text {
			t.Errorf("case %d text:\n got: %q\nwant: %q", i, got, c.Text)
		}
	}
}

func firstDiff(a, b string) string {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			lo := i - 40
			if lo < 0 {
				lo = 0
			}
			hi := i + 60
			if hi > n {
				hi = n
			}
			return fmt.Sprintf("at %d:\n got:  %q\n want: %q", i, a[lo:hi], b[lo:hi])
		}
	}
	return "one is a prefix of the other"
}

func TestTokenHelpersMatchNode(t *testing.T) {
	v := load(t)
	if passhash.DigestToken("hello") != v.TokenHash {
		t.Fatal("token digest differs")
	}
	if got := SQLiteTime(time.UnixMilli(1700000000123)); got != v.SqliteDate {
		t.Fatalf("%s != %s", got, v.SqliteDate)
	}
}

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestConfig(t *testing.T) {
	c := LoadConfig("NEOAGENT", env(nil), "NeoAgent", "https://x.example/")
	if c.Enabled || len(c.Missing) != 2 || c.Port != 587 || !c.RequireTLS || c.Secure || c.RequireSignupConfirmation ||
		c.BrandName != "NeoAgent" || c.PublicURL != "https://x.example" || c.TokenTTLHours != 24 || !c.NotifyUnusualLogin {
		t.Fatalf("defaults: %+v", c)
	}
	c = LoadConfig("NEOAGENT", env(map[string]string{
		"NEOAGENT_EMAIL_SMTP_HOST": "smtp.example", "NEOAGENT_EMAIL_FROM": "a@b.c", "NEOAGENT_EMAIL_SMTP_PORT": "465",
		"NEOAGENT_EMAIL_PUBLIC_URL": "https://pub.example//", "NEOAGENT_EMAIL_TOKEN_TTL_HOURS": "2",
		"NEOAGENT_EMAIL_BRAND_NAME": "Acme", "NEOAGENT_EMAIL_REQUIRE_SIGNUP_CONFIRMATION": "false",
	}), "NeoAgent", "ignored")
	if !c.Enabled || !c.Secure || c.RequireTLS || c.RequireSignupConfirmation || !c.RequireChangeConfirmation ||
		c.BrandName != "Acme" || c.TokenTTLHours != 2 || strings.HasSuffix(c.PublicURL, "/") && c.PublicURL != "https://pub.example/" {
		t.Fatalf("configured: %+v", c)
	}
	// Another app uses its own prefix.
	c = LoadConfig("NEOCLOUD", env(map[string]string{"NEOCLOUD_EMAIL_SMTP_HOST": "h", "NEOCLOUD_EMAIL_FROM": "f@x.y"}), "NeoCloud", "")
	if !c.Enabled || c.BrandName != "NeoCloud" {
		t.Fatalf("prefix: %+v", c)
	}
}

func tokenDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+t.TempDir()+"/t.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	db.SetMaxOpenConns(1)
	for _, q := range []string{
		`CREATE TABLE users (id INTEGER PRIMARY KEY, username TEXT, email TEXT)`,
		`CREATE TABLE user_email_tokens (id INTEGER PRIMARY KEY AUTOINCREMENT, user_id INTEGER NOT NULL, type TEXT NOT NULL,
			email TEXT NOT NULL, token_hash TEXT UNIQUE NOT NULL, created_at TEXT DEFAULT (datetime('now')),
			expires_at TEXT NOT NULL, consumed_at TEXT)`,
		`INSERT INTO users VALUES (1, 'neo', 'neo@example.com')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func TestTokensCreateFindAndSupersede(t *testing.T) {
	db := tokenDB(t)
	tk := Tokens{DB: db}
	ctx := context.Background()

	first, _, err := tk.Create(ctx, "1", "password_reset", "neo@example.com", time.Hour)
	if err != nil || len(first) != 43 {
		t.Fatalf("%q %v", first, err)
	}
	var stored string
	db.QueryRow(`SELECT token_hash FROM user_email_tokens`).Scan(&stored)
	if stored != passhash.DigestToken(first) || strings.Contains(stored, first) {
		t.Fatal("only the digest may be stored")
	}
	row, err := tk.Find(ctx, db, first, "password_reset")
	if err != nil || row == nil || row.Username != "neo" || row.Email != "neo@example.com" || row.CurrentEmail.String != "neo@example.com" {
		t.Fatalf("%+v %v", row, err)
	}
	if r, _ := tk.Find(ctx, db, first, "signup_confirmation"); r != nil {
		t.Fatal("wrong type must not match")
	}
	if r, _ := tk.Find(ctx, db, first, ""); r == nil {
		t.Fatal("empty type accepts any")
	}
	if r, _ := tk.Find(ctx, db, "nope", ""); r != nil {
		t.Fatal("unknown token")
	}

	second, _, _ := tk.Create(ctx, "1", "password_reset", "neo@example.com", time.Hour)
	if r, _ := tk.Find(ctx, db, first, ""); r != nil {
		t.Fatal("a newer token retires the older one")
	}
	if r, _ := tk.Find(ctx, db, second, ""); r == nil {
		t.Fatal("newest token is live")
	}
	other, _, _ := tk.Create(ctx, "1", "signup_confirmation", "neo@example.com", time.Hour)
	if r, _ := tk.Find(ctx, db, second, ""); r == nil {
		t.Fatal("tokens of another type are independent")
	}
	_ = other

	expired, _, _ := tk.Create(ctx, "1", "email_change_confirmation", "x@example.com", -time.Minute)
	if r, _ := tk.Find(ctx, db, expired, ""); r != nil {
		t.Fatal("expired token must not be found")
	}
}

func TestAttemptLimiter(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	l := &AttemptLimiter{Now: func() time.Time { return now }}
	for i := 0; i < 12; i++ {
		if err := l.Allowed("k"); err != nil {
			t.Fatalf("try %d: %v", i, err)
		}
		l.Register("k")
	}
	if err := l.Allowed("k"); err != ErrTooManyAttempts {
		t.Fatalf("12 failures lock the token: %v", err)
	}
	if err := l.Allowed("other"); err != nil {
		t.Fatal("per key")
	}
	now = now.Add(16 * time.Minute)
	if err := l.Allowed("k"); err != nil {
		t.Fatalf("window expiry unlocks: %v", err)
	}
	l.Register("k")
	l.Reset("k")
	if _, ok := l.state["k"]; ok {
		t.Fatal("reset forgets the key")
	}
}

// fakeSMTP is a tiny plain-text SMTP server that records one message.
type fakeSMTP struct {
	ln   net.Listener
	mu   sync.Mutex
	data string
	from string
	rcpt []string
}

func startSMTP(t *testing.T) *fakeSMTP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &fakeSMTP{ln: ln}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(c)
		}
	}()
	return s
}

func (s *fakeSMTP) serve(c net.Conn) {
	defer c.Close()
	r := bufio.NewReader(c)
	fmt.Fprint(c, "220 fake ESMTP\r\n")
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		cmd := strings.ToUpper(strings.TrimSpace(line))
		switch {
		case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
			fmt.Fprint(c, "250 fake\r\n")
		case strings.HasPrefix(cmd, "MAIL FROM"):
			s.mu.Lock()
			s.from = strings.TrimSpace(line)
			s.mu.Unlock()
			fmt.Fprint(c, "250 ok\r\n")
		case strings.HasPrefix(cmd, "RCPT TO"):
			s.mu.Lock()
			s.rcpt = append(s.rcpt, strings.TrimSpace(line))
			s.mu.Unlock()
			fmt.Fprint(c, "250 ok\r\n")
		case cmd == "DATA":
			fmt.Fprint(c, "354 go\r\n")
			var b strings.Builder
			for {
				l, err := r.ReadString('\n')
				if err != nil || l == ".\r\n" {
					break
				}
				b.WriteString(l)
			}
			s.mu.Lock()
			s.data = b.String()
			s.mu.Unlock()
			fmt.Fprint(c, "250 queued\r\n")
		case cmd == "QUIT":
			fmt.Fprint(c, "221 bye\r\n")
			return
		default:
			fmt.Fprint(c, "250 ok\r\n")
		}
	}
}

func TestSenderDeliversMultipartMail(t *testing.T) {
	srv := startSMTP(t)
	host, portStr, _ := net.SplitHostPort(srv.ln.Addr().String())
	var port int
	fmt.Sscan(portStr, &port)
	cfg := Config{Enabled: true, Host: host, Port: port, From: "NeoAgent <noreply@example.com>", ReplyTo: "help@example.com",
		BrandName: "NeoAgent", SupportURL: "https://example.com/help"}
	err := Sender{Cfg: cfg}.Send(context.Background(), "neo@example.com", Message{
		Subject: "Reset your NeoAgent password", Title: "Reset your password", Intro: "Use the link.",
		ActionURL: "https://x/reset?token=t", ActionLabel: "Reset password",
		Details: []Detail{{"Account", "neo"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	// Bodies are quoted-printable encoded, so '=' travels as '=3D'.
	data := strings.ReplaceAll(strings.ReplaceAll(srv.data, "=\r\n", ""), "=3D", "=")
	for _, want := range []string{
		"Subject: Reset your NeoAgent password", "To: <neo@example.com>", "Reply-To: <help@example.com>",
		"multipart/alternative", "text/plain", "text/html", "Reset password: https://x/reset?token=t",
		"Support: https://example.com/help",
	} {
		if !strings.Contains(data, want) {
			t.Errorf("message lacks %q", want)
		}
	}
	if !strings.Contains(srv.from, "noreply@example.com") || len(srv.rcpt) != 1 || !strings.Contains(srv.rcpt[0], "neo@example.com") {
		t.Fatalf("envelope: from=%q rcpt=%v", srv.from, srv.rcpt)
	}
}

func TestSenderRefusesWhenDisabledAndHeaderInjection(t *testing.T) {
	if err := (Sender{}).Send(context.Background(), "a@b.c", Message{Subject: "x"}); err != ErrNotConfigured {
		t.Fatalf("%v", err)
	}
	cfg := Config{Enabled: true, Host: "127.0.0.1", Port: 1, From: "a@b.c"}
	if err := (Sender{Cfg: cfg}).Send(context.Background(), "a@b.c", Message{Subject: "Hi\r\nBcc: evil@x.y"}); err == nil || !strings.Contains(err.Error(), "single line") {
		t.Fatalf("header injection must be refused: %v", err)
	}
	if err := (Sender{Cfg: cfg}).Send(context.Background(), "a@b.c\r\nBcc: evil@x.y", Message{Subject: "x"}); err == nil {
		t.Fatal("recipient injection must be refused")
	}
}

func TestTokensConsumeRetireRecentPending(t *testing.T) {
	db := tokenDB(t)
	tk := Tokens{DB: db}
	ctx := context.Background()

	reset, _, _ := tk.Create(ctx, "1", "password_reset", "neo@example.com", time.Hour)
	row, _ := tk.Find(ctx, db, reset, "password_reset")
	if err := tk.Consume(ctx, db, row.ID); err != nil {
		t.Fatal(err)
	}
	if err := tk.Consume(ctx, db, row.ID); err != ErrTokenSpent {
		t.Fatalf("a token is spent once: %v", err)
	}
	expired, _, _ := tk.Create(ctx, "1", "signup_confirmation", "neo@example.com", -time.Minute)
	_ = expired
	var expiredID int64
	db.QueryRow(`SELECT id FROM user_email_tokens WHERE type = 'signup_confirmation'`).Scan(&expiredID)
	if err := tk.Consume(ctx, db, expiredID); err != ErrTokenSpent {
		t.Fatalf("an expired token cannot be spent: %v", err)
	}

	if n, err := tk.Recent(ctx, "1", "password_reset", time.Now().Add(-time.Minute)); err != nil || n != 1 {
		t.Fatalf("recent %d %v", n, err)
	}
	if n, _ := tk.Recent(ctx, "1", "password_reset", time.Now().Add(time.Hour)); n != 0 {
		t.Fatalf("recent counts only since the given time: %d", n)
	}

	if p, err := tk.Pending(ctx, "1", "email_change_confirmation"); err != nil || p != "" {
		t.Fatalf("no pending change: %q %v", p, err)
	}
	tk.Create(ctx, "1", "email_change_confirmation", "new@example.com", time.Hour)
	if p, _ := tk.Pending(ctx, "1", "email_change_confirmation"); p != "new@example.com" {
		t.Fatalf("pending %q", p)
	}
	keep, _, _ := tk.Create(ctx, "1", "password_reset", "neo@example.com", time.Hour)
	if err := tk.Retire(ctx, db, "1", "email_change_confirmation"); err != nil {
		t.Fatal(err)
	}
	if p, _ := tk.Pending(ctx, "1", "email_change_confirmation"); p != "" {
		t.Fatal("retired change still pending")
	}
	if r, _ := tk.Find(ctx, db, keep, ""); r == nil {
		t.Fatal("retire touched another type")
	}
	if err := tk.Retire(ctx, db, "1"); err != nil {
		t.Fatal(err)
	}
	if r, _ := tk.Find(ctx, db, keep, ""); r != nil {
		t.Fatal("retire without types spends all")
	}
}
