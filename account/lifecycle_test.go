package account

import (
	"archive/zip"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/NeoLabs-Systems/neolabs-go/userid"
)

// withLifecycle wires export and deletion the way an app would: notes rows
// with files under the app's file root.
func withLifecycle(a *fakeApp, format Format, rule AdminRule) {
	a.h.Exporter = &Exporter{
		Format: format,
		Filename: func(acc Account, now time.Time) string {
			return "fake-" + acc.Username + "/" + now.Format("2006-01-02") + ".json"
		},
		Head: func(_ context.Context, acc Account, now time.Time) ([]Field, error) {
			return []Field{{"format", 1}, {"kind", "fake-account"}, {"exportedAt", now.UTC().Format(time.RFC3339)}, {"userId", acc.ID}}, nil
		},
		Sections: []Section{
			{Key: "profile", Value: func(ctx context.Context, acc Account) (any, error) {
				var m map[string]any
				raw := `{"username":"` + acc.Username + `","password":"$2a$hash","nested":{"api_key":"k","ok":1}}`
				json.Unmarshal([]byte(raw), &m)
				return (&Redactor{}).Map(m), nil
			}},
			{Key: "notes", Value: func(ctx context.Context, acc Account) (any, error) {
				return Rows{DB: a.db.R, Query: `SELECT id, body, token_hash, picture FROM notes WHERE user_id = ? ORDER BY id`,
					Args: []any{string(acc.ID)}, Redact: &Redactor{}}, nil
			}},
		},
		Files: func(ctx context.Context, acc Account, add func(File) error) error {
			rows, err := a.db.R.QueryContext(ctx, `SELECT file FROM notes WHERE user_id = ? AND file IS NOT NULL`, string(acc.ID))
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var name string
				rows.Scan(&name)
				if err := add(File{Name: "files/" + name, Open: func() (io.ReadCloser, error) { return os.Open(filepath.Join(a.dir, name)) }}); err != nil {
					return err
				}
			}
			return rows.Err()
		},
	}
	a.h.Deleter = &Deleter{
		DB:   a.db.W,
		Rule: rule,
		OtherAdmins: func(ctx context.Context, tx *sql.Tx, id userid.ID) (int, error) {
			var n int
			err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE is_admin = 1 AND disabled_at IS NULL AND id <> ?`, string(id)).Scan(&n)
			return n, err
		},
		Steps: []Step{
			func(ctx context.Context, tx *sql.Tx, d *Deletion) error {
				rows, err := tx.QueryContext(ctx, `SELECT file FROM notes WHERE user_id = ? AND file IS NOT NULL`, string(d.Account.ID))
				if err != nil {
					return err
				}
				defer rows.Close()
				for rows.Next() {
					var name string
					rows.Scan(&name)
					d.RemoveAfterCommit(a.dir, filepath.Join(a.dir, name))
				}
				return rows.Err()
			},
			DeleteWhere("user_id", "notes"),
			DeleteWhere("id", "users"),
		},
		Sessions: a.sessions,
	}
	a.mux.Handle("GET /api/account/export", a.h.Export(false))
	a.mux.Handle("POST /api/account/export", a.h.Export(true))
	a.mux.Handle("POST /api/account/delete", a.h.DeleteAccount(DeleteOptions{ConfirmUsername: true, Reauth: true,
		Done: func(w http.ResponseWriter, r *http.Request, d *Deletion) {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "rowsDeleted": d.Result["rowsDeleted"]})
		}}))
	a.mux.Handle("DELETE /api/admin/users/{id}", a.h.AdminDeleteAccount(func(r *http.Request) (userid.ID, error) {
		return userid.ID(r.PathValue("id")), nil
	}, nil))
}

func (a *fakeApp) addNote(id userid.ID, body, file string) {
	a.t.Helper()
	var f any
	if file != "" {
		f = file
		path := filepath.Join(a.dir, file)
		os.MkdirAll(filepath.Dir(path), 0o700)
		if err := os.WriteFile(path, []byte("content of "+file), 0o600); err != nil {
			a.t.Fatal(err)
		}
	}
	if _, err := a.db.W.Exec(`INSERT INTO notes (user_id, body, file, token_hash, picture) VALUES (?, ?, ?, 'secret-digest', x'0102')`, string(id), body, f); err != nil {
		a.t.Fatal(err)
	}
}

func TestExportJSONCarriesSectionsButNoSecrets(t *testing.T) {
	each(t, func(t *testing.T, a *fakeApp) {
		withLifecycle(a, JSON, KeepLastAdmin)
		id := a.addUser("neo", false, false)
		other := a.addUser("trin", false, false)
		a.addNote(id, `<b>mine</b>`, "")
		a.addNote(other, "theirs", "")

		r := a.do("GET", "/api/account/export", id, "", nil)
		if r.Code != 200 || r.Header.Get("Content-Type") != "application/json; charset=utf-8" || r.Header.Get("Cache-Control") != "no-store" {
			t.Fatalf("%d %v %s", r.Code, r.Header, r.Body)
		}
		if cd := r.Header.Get("Content-Disposition"); !strings.HasPrefix(cd, `attachment; filename="fake-neo-`) || strings.Contains(cd, "/") {
			t.Fatalf("unsafe filename %q", cd)
		}
		for _, leak := range []string{"secret-digest", "$2a$hash", `"k"`, "theirs"} {
			if strings.Contains(r.Body, leak) {
				t.Fatalf("export leaks %q: %s", leak, r.Body)
			}
		}
		if !strings.HasPrefix(r.Body, `{"format":1,"kind":"fake-account","exportedAt":`) || !strings.Contains(r.Body, `"body":"<b>mine</b>"`) {
			t.Fatalf("order or escaping: %s", r.Body)
		}
		m := r.json(t)
		if a.uuid && m["userId"] != string(id) || !a.uuid && m["userId"] != float64(1) {
			t.Fatalf("user id %v", m["userId"])
		}
		notes := m["notes"].([]any)
		if len(notes) != 1 || notes[0].(map[string]any)["token_hash"] != Redacted || notes[0].(map[string]any)["picture"] != "AQI=" {
			t.Fatalf("%v", notes)
		}
		if p := m["profile"].(map[string]any); p["password"] != Redacted || p["nested"].(map[string]any)["api_key"] != Redacted {
			t.Fatalf("%v", p)
		}
	})
}

func TestExportNestsSectionsAndNeedsReauthWhenAsked(t *testing.T) {
	a := newFakeApp(t, false)
	withLifecycle(a, JSON, KeepLastAdmin)
	a.h.Exporter.Nest = "data"
	id := a.addUser("neo", false, true)
	if r := a.do("POST", "/api/account/export", id, "", map[string]any{"currentPassword": "correct horse battery"}); r.Code != 401 || r.json(t)["code"] != "TWO_FACTOR_REQUIRED" {
		t.Fatalf("%d %s", r.Code, r.Body)
	}
	r := a.do("POST", "/api/account/export", id, "", map[string]any{"currentPassword": "correct horse battery", "code": goodCode})
	if r.Code != 200 {
		t.Fatalf("%d %s", r.Code, r.Body)
	}
	m := r.json(t)
	if _, ok := m["data"].(map[string]any)["notes"]; !ok || m["notes"] != nil {
		t.Fatalf("%s", r.Body)
	}
	if r := a.do("GET", "/api/account/export", "", "", nil); r.Code != 401 {
		t.Fatal(r.Code)
	}
}

func TestExportZipHasManifestDataAndFiles(t *testing.T) {
	a := newFakeApp(t, true)
	withLifecycle(a, Zip, KeepLastAdmin)
	id := a.addUser("neo", false, false)
	a.addNote(id, "with file", "neo/a.txt")
	r := a.do("GET", "/api/account/export", id, "", nil)
	if r.Code != 200 || r.Header.Get("Content-Type") != "application/zip" {
		t.Fatalf("%d %s", r.Code, r.Body)
	}
	z, err := zip.NewReader(bytes.NewReader([]byte(r.Body)), int64(len(r.Body)))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, f := range z.File {
		rc, _ := f.Open()
		b, _ := io.ReadAll(rc)
		rc.Close()
		got[f.Name] = string(b)
	}
	if !strings.Contains(got["manifest.json"], `"kind":"fake-account"`) || !strings.Contains(got["data.json"], `"body":"with file"`) ||
		got["files/neo/a.txt"] != "content of neo/a.txt" || len(got) != 3 {
		t.Fatalf("%v", got)
	}
	for _, bad := range []string{"../x", "/etc/passwd", "manifest.json", ""} {
		if _, err := archiveName(bad); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
}

func TestExportFailureBeforeTheDownloadAnswersAnError(t *testing.T) {
	a := newFakeApp(t, false)
	withLifecycle(a, JSON, KeepLastAdmin)
	a.h.Exporter.Sections = append(a.h.Exporter.Sections, Section{Key: "broken", Value: func(context.Context, Account) (any, error) {
		return nil, errors.New("table missing at /secret/path")
	}})
	id := a.addUser("neo", false, false)
	r := a.do("GET", "/api/account/export", id, "", nil)
	if r.Code != 500 || r.Header.Get("Content-Disposition") != "" || strings.Contains(r.Body, "/secret") {
		t.Fatalf("%d %v %s", r.Code, r.Header, r.Body)
	}
}

func TestExportFailureMidDownloadAbortsTheConnection(t *testing.T) {
	a := newFakeApp(t, false)
	withLifecycle(a, JSON, KeepLastAdmin)
	big := strings.Repeat("x", 100<<10)
	a.h.Exporter.Sections = []Section{
		{Key: "big", Value: func(context.Context, Account) (any, error) { return big, nil }},
		{Key: "broken", Value: func(context.Context, Account) (any, error) { return nil, errors.New("boom") }},
	}
	id := a.addUser("neo", false, false)
	defer func() {
		if rec := recover(); rec != http.ErrAbortHandler {
			t.Fatalf("expected an aborted response, got %v", rec)
		}
	}()
	a.do("GET", "/api/account/export", id, "", nil)
}

func TestDeleteAccountErasesAfterConfirmationAndReauth(t *testing.T) {
	each(t, func(t *testing.T, a *fakeApp) {
		withLifecycle(a, JSON, KeepLastAdmin)
		id := a.addUser("neo", false, false)
		other := a.addUser("trin", false, false)
		a.addNote(id, "mine", "neo/a.txt")
		a.addNote(other, "theirs", "trin/b.txt")
		sid := a.login(id)
		otherSid := a.login(other)
		a.rec.RequestReset(context.Background(), "neo")

		del := func(body map[string]any) reply { return a.do("POST", "/api/account/delete", id, sid, body) }
		if r := del(map[string]any{"confirmUsername": "trin", "currentPassword": "correct horse battery"}); r.json(t)["code"] != "CONFIRMATION_MISMATCH" {
			t.Fatalf("%s", r.Body)
		}
		if r := del(map[string]any{"confirmUsername": " NEO ", "currentPassword": "wrong"}); r.Code != 401 {
			t.Fatalf("%s", r.Body)
		}
		if a.count(`SELECT COUNT(*) FROM users WHERE id = ?`, string(id)) != 1 {
			t.Fatal("a refused delete erased the account")
		}
		r := del(map[string]any{"confirmUsername": " NEO ", "currentPassword": "correct horse battery"})
		if r.Code != 200 || r.json(t)["rowsDeleted"] != float64(2) {
			t.Fatalf("%d %s", r.Code, r.Body)
		}
		if a.count(`SELECT COUNT(*) FROM users WHERE id = ?`, string(id)) != 0 ||
			a.count(`SELECT COUNT(*) FROM notes WHERE user_id = ?`, string(id)) != 0 ||
			a.count(`SELECT COUNT(*) FROM user_email_tokens`) != 0 {
			t.Fatal("rows survived")
		}
		if _, err := os.Stat(filepath.Join(a.dir, "neo/a.txt")); !os.IsNotExist(err) {
			t.Fatal("the account's file survived")
		}
		if _, err := os.Stat(filepath.Join(a.dir, "trin/b.txt")); err != nil {
			t.Fatal("another account's file was removed")
		}
		if d, _ := a.sessions.Get(context.Background(), sid); d != nil {
			t.Fatal("the session survived")
		}
		if d, _ := a.sessions.Get(context.Background(), otherSid); d == nil {
			t.Fatal("another account's session ended")
		}
	})
}

func TestDeleteRollsBackAndKeepsFilesWhenAStepFails(t *testing.T) {
	a := newFakeApp(t, false)
	withLifecycle(a, JSON, KeepLastAdmin)
	a.h.Deleter.Steps = append(a.h.Deleter.Steps, func(context.Context, *sql.Tx, *Deletion) error { return errors.New("step failed") })
	id := a.addUser("neo", false, false)
	a.addNote(id, "mine", "neo/a.txt")
	acc, _ := a.Account(context.Background(), id)
	if _, err := a.h.Deleter.Delete(context.Background(), acc); err == nil {
		t.Fatal("expected failure")
	}
	if a.count(`SELECT COUNT(*) FROM notes`) != 1 || a.count(`SELECT COUNT(*) FROM users`) != 1 {
		t.Fatal("rows erased despite rollback")
	}
	if _, err := os.Stat(filepath.Join(a.dir, "neo/a.txt")); err != nil {
		t.Fatal("files go only after the commit")
	}
}

func TestLastAdminAndAdminRules(t *testing.T) {
	ctx := context.Background()
	a := newFakeApp(t, false)
	withLifecycle(a, JSON, KeepLastAdmin)
	root := a.addUser("root", true, false)
	acc, _ := a.Account(ctx, root)
	if _, err := a.h.Deleter.Delete(ctx, acc); err != ErrLastAdmin {
		t.Fatalf("the last admin deleted itself: %v", err)
	}
	second := a.addUser("second", true, false)
	user := a.addUser("user", false, false)
	if r := a.do("DELETE", "/api/admin/users/"+string(root), root, "", nil); r.json(t)["code"] != "CANNOT_DELETE_SELF" {
		t.Fatal(r.Body)
	}
	if r := a.do("DELETE", "/api/admin/users/"+string(user), root, "", nil); r.Code != 204 {
		t.Fatal(r.Code, r.Body)
	}
	if r := a.do("DELETE", "/api/admin/users/"+string(user), root, "", nil); r.Code != 404 {
		t.Fatal(r.Code, r.Body)
	}
	if _, err := a.h.Deleter.Delete(ctx, acc); err != nil {
		t.Fatalf("an admin may go while another remains: %v", err)
	}
	last, _ := a.Account(ctx, second)
	if _, err := a.h.Deleter.Delete(ctx, last); err != ErrLastAdmin {
		t.Fatal(err)
	}
	third := a.addUser("third", true, false)
	a.h.Deleter.ProtectAdminTargets = true
	if r := a.do("DELETE", "/api/admin/users/"+string(third), second, "", nil); r.json(t)["code"] != "ADMIN_ACCOUNT" {
		t.Fatal(r.Body)
	}
	a.h.Deleter.ProtectAdminTargets = false
	if r := a.do("DELETE", "/api/admin/users/"+string(third), second, "", nil); r.Code != 204 {
		t.Fatal(r.Code, r.Body)
	}

	b := newFakeApp(t, true)
	withLifecycle(b, JSON, KeepAdmins)
	r1 := b.addUser("root", true, false)
	r2 := b.addUser("other", true, false)
	if r := b.do("POST", "/api/account/delete", r1, "", map[string]any{"confirmUsername": "root", "currentPassword": "correct horse battery"}); r.json(t)["code"] != "ADMIN_SELF_DELETE" {
		t.Fatal(r.Body)
	}
	if r := b.do("DELETE", "/api/admin/users/"+string(r2), r1, "", nil); r.json(t)["code"] != "ADMIN_ACCOUNT" {
		t.Fatal(r.Body)
	}
}

func TestRemoveInsideStaysInTheRoot(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "keep.txt")
	os.WriteFile(outside, []byte("x"), 0o600)
	for _, name := range []string{outside, "../" + filepath.Base(filepath.Dir(outside)) + "/keep.txt"} {
		if err := removeInside(root, name); err == nil {
			t.Fatalf("%q outside the root was accepted", name)
		}
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatal("a file outside the root was removed")
	}
	os.Symlink(filepath.Dir(outside), filepath.Join(root, "link"))
	removeInside(root, "link/keep.txt")
	if _, err := os.Stat(outside); err != nil {
		t.Fatal("a symlink led the removal out of the root")
	}
	if err := removeInside(filepath.Join(root, "missing"), "x"); err != nil {
		t.Fatal(err)
	}
}

func TestDeleteWhereRefusesOddIdentifiers(t *testing.T) {
	a := newFakeApp(t, false)
	id := a.addUser("neo", false, false)
	acc, _ := a.Account(context.Background(), id)
	x := &Deleter{DB: a.db.W, Steps: []Step{DeleteWhere("user_id", `notes"; DROP TABLE users; --`)}}
	if _, err := x.Delete(context.Background(), acc); err == nil {
		t.Fatal("injected table name accepted")
	}
	if _, err := (&Deleter{DB: a.db.W}).Delete(context.Background(), acc); err == nil {
		t.Fatal("a deleter without steps must refuse")
	}
}
