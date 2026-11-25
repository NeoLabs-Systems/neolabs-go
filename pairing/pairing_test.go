package pairing

import (
	"context"
	"encoding/json"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NeoLabs-Systems/neolabs-go/apperr"
	"github.com/NeoLabs-Systems/neolabs-go/dbx"
	"github.com/NeoLabs-Systems/neolabs-go/passhash"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	db, err := dbx.Open(context.Background(), filepath.Join(t.TempDir(), "p.db"), dbx.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	for _, q := range []string{
		`CREATE TABLE users (id INTEGER PRIMARY KEY, username TEXT)`,
		`INSERT INTO users VALUES (1, 'neo')`,
		`CREATE TABLE user_device_pairings (id INTEGER PRIMARY KEY AUTOINCREMENT, user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			code_hash TEXT NOT NULL UNIQUE, status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','used')), device_label TEXT,
			expires_at TEXT NOT NULL, used_at TEXT, created_at TEXT NOT NULL DEFAULT (datetime('now')))`,
	} {
		if _, err := db.W.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	return &Store{DB: db, Scheme: "neoagent"}
}

func code(err error) (int, string) {
	if e, ok := apperr.As(err); ok {
		return e.Status, e.Code
	}
	return -1, ""
}

func TestFlow(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	st, err := s.Start(ctx, 1, "https://agent.example.com/base")
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(st.QRPayload)
	if err != nil || u.Scheme != "neoagent" || u.Host != "pair" || u.Query().Get("server") != "https://agent.example.com/base" || u.Query().Get("code") != st.Code {
		t.Fatalf("payload %q: %v", st.QRPayload, err)
	}
	// Node's URLSearchParams keeps insertion order: server first, then code.
	if !strings.HasPrefix(st.QRPayload, "neoagent://pair?server=https%3A%2F%2Fagent.example.com%2Fbase&code=") {
		t.Fatalf("payload encoding: %q", st.QRPayload)
	}
	var stored string
	s.DB.W.QueryRow(`SELECT code_hash FROM user_device_pairings`).Scan(&stored)
	if stored != passhash.DigestToken(st.Code) {
		t.Fatal("only the digest is stored")
	}

	got, _ := s.Status(ctx, 1, st.Code)
	if raw, _ := json.Marshal(got); string(raw) != `{"status":"pending"}` {
		t.Fatalf("%s", raw)
	}
	if _, err := s.Status(ctx, 2, st.Code); err == nil {
		t.Fatal("another account must not see the code")
	}

	uid, err := s.Redeem(ctx, st.Code, "  Neo's   iPhone ")
	if err != nil || uid != 1 {
		t.Fatalf("%d %v", uid, err)
	}
	got, _ = s.Status(ctx, 1, st.Code)
	if raw, _ := json.Marshal(got); string(raw) != `{"status":"used","device":{"name":"Neo's iPhone"}}` {
		t.Fatalf("%s", raw)
	}
	if _, err := s.Redeem(ctx, st.Code, "again"); err == nil {
		t.Fatal("a code works once")
	} else if status, c := code(err); status != 410 || c != "EXPIRED_TOKEN" {
		t.Fatalf("%v", err)
	}
}

func TestUnknownEmptyAndExpiredCodes(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	for _, c := range []string{"", "nope"} {
		if _, err := s.Redeem(ctx, c, ""); err == nil {
			t.Fatalf("%q", c)
		} else if status, _ := code(err); status != 410 {
			t.Fatalf("%q: %v", c, err)
		}
	}
	if _, err := s.Status(ctx, 1, "nope"); err == nil {
		t.Fatal("unknown code status")
	} else if status, c := code(err); status != 404 || c != "NOT_FOUND" {
		t.Fatal(err)
	}
	st, _ := s.Start(ctx, 1, "https://x")
	s.DB.W.Exec(`UPDATE user_device_pairings SET expires_at = datetime('now', '-1 minute')`)
	if got, _ := s.Status(ctx, 1, st.Code); got.Status != "expired" {
		t.Fatalf("%+v", got)
	}
	if _, err := s.Redeem(ctx, st.Code, ""); err == nil {
		t.Fatal("expired code redeemed")
	}
}

func TestOpenCodeLimit(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if _, err := s.Start(ctx, 1, "https://x"); err != nil {
			t.Fatal(err)
		}
	}
	_, err := s.Start(ctx, 1, "https://x")
	if status, c := code(err); status != 429 || c != "TOO_MANY_PAIRING_CODES" {
		t.Fatalf("%v", err)
	}
}

func TestUsedWithoutLabelShowsNullDevice(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	st, _ := s.Start(ctx, 1, "https://x")
	if _, err := s.Redeem(ctx, st.Code, "   "); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Status(ctx, 1, st.Code)
	if raw, _ := json.Marshal(got); string(raw) != `{"status":"used","device":null}` {
		t.Fatalf("%s", raw)
	}
}
