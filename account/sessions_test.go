package account

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"testing"

	"github.com/NeoLabs-Systems/neolabs-go/httpx"
	"github.com/NeoLabs-Systems/neolabs-go/userid"
)

func TestSessionListRevokeAndSignOutEverywhere(t *testing.T) {
	each(t, func(t *testing.T, a *fakeApp) {
		id := a.addUser("neo", false, false)
		other := a.addUser("trin", false, false)
		cur, s2, s3 := a.login(id), a.login(id), a.login(id)
		foreign := a.login(other)

		list := a.do("GET", "/api/account/sessions", id, cur, nil)
		var body struct {
			Sessions []map[string]any `json:"sessions"`
		}
		json.Unmarshal([]byte(list.Body), &body)
		if list.Code != 200 || len(body.Sessions) != 3 {
			t.Fatalf("%d %s", list.Code, list.Body)
		}
		currents := 0
		for _, s := range body.Sessions {
			if s["current"] == true {
				currents++
			}
			if s["userAgent"] != "test" || s["createdAt"] == nil || s["expiresAt"] == nil || s["ipAddress"] != nil {
				t.Fatalf("%v", s)
			}
			for _, sid := range []string{cur, s2, s3} {
				if s["id"] == sid {
					t.Fatal("store ids must not leave the server")
				}
			}
		}
		if currents != 1 {
			t.Fatalf("%d current sessions", currents)
		}

		if r := a.do("DELETE", "/api/account/sessions/"+PublicSessionID(cur), id, cur, nil); r.Code != 400 || r.json(t)["code"] != "CURRENT_SESSION" {
			t.Fatalf("%d %s", r.Code, r.Body)
		}
		if r := a.do("DELETE", "/api/account/sessions/"+PublicSessionID(foreign), id, cur, nil); r.Code != 404 {
			t.Fatalf("another account's session: %d", r.Code)
		}
		r := a.do("DELETE", "/api/account/sessions/"+PublicSessionID(s2), id, cur, nil)
		if r.Code != 200 || r.json(t)["success"] != true || len(r.json(t)["sessions"].([]any)) != 2 {
			t.Fatalf("%d %s", r.Code, r.Body)
		}

		r = a.do("POST", "/api/account/sessions/revoke-all", id, cur, nil)
		if r.Code != 200 || len(r.json(t)["sessions"].([]any)) != 1 {
			t.Fatalf("%d %s", r.Code, r.Body)
		}
		ctx := context.Background()
		if d, _ := a.sessions.Get(ctx, s3); d != nil {
			t.Fatal("other session survived")
		}
		if d, _ := a.sessions.Get(ctx, cur); d == nil {
			t.Fatal("the caller's session ended")
		}
		if r := a.do("POST", "/api/auth/logout-all", id, cur, nil); r.Code != 204 {
			t.Fatal(r.Code)
		}
		if d, _ := a.sessions.Get(ctx, cur); d != nil {
			t.Fatal("logout-all keeps nothing")
		}
		if d, _ := a.sessions.Get(ctx, foreign); d == nil {
			t.Fatal("another account was signed out")
		}
	})
}

func TestSessionJSONWritesNumericIDsAsNumbers(t *testing.T) {
	raw, _ := json.Marshal([]Session{{ID: "42", Current: true, Extra: map[string]any{"location": "Berlin"}}, {ID: "abc"}})
	want := `[{"createdAt":null,"current":true,"expiresAt":null,"id":42,"ipAddress":null,"lastSeenAt":null,"location":"Berlin","userAgent":null},` +
		`{"createdAt":null,"current":false,"expiresAt":null,"id":"abc","ipAddress":null,"lastSeenAt":null,"userAgent":null}]`
	if string(raw) != want {
		t.Fatalf("%s", raw)
	}
}

// An app with a nested envelope and its own codes keeps both.
func TestAppEnvelopeAndCodesAreKept(t *testing.T) {
	a := newFakeApp(t, true)
	a.h.WriteError = func(w http.ResponseWriter, _ *http.Request, err error) {
		p := Describe(err)
		if errors.Is(err, ErrWrongPassword) {
			p.Code = "INVALID_PASSWORD"
		}
		httpx.WriteJSON(w, p.Status, map[string]any{"error": map[string]any{"code": p.Code, "message": p.Message}})
	}
	a.h.Fields = Fields{Password: "password", Code: "twoFactorCode"}
	withLifecycle(a, JSON, KeepLastAdmin)
	id := a.addUser("neo", false, false)
	r := a.do("POST", "/api/account/export", id, "", map[string]any{"password": "nope"})
	if r.Code != 401 || r.Body != `{"error":{"code":"INVALID_PASSWORD","message":"Current password is incorrect."}}` {
		t.Fatalf("%d %s", r.Code, r.Body)
	}
	if r := a.do("POST", "/api/account/export", id, "", map[string]any{"password": "correct horse battery"}); r.Code != 200 {
		t.Fatalf("%d %s", r.Code, r.Body)
	}
	if r := a.do("POST", "/api/account/export", id, "", "not an object"); r.Code != 400 {
		t.Fatalf("%d %s", r.Code, r.Body)
	}
}

func TestConcurrentAdminSelfDeletesKeepOneAdmin(t *testing.T) {
	a := newFakeApp(t, false)
	withLifecycle(a, JSON, KeepLastAdmin)
	ctx := context.Background()
	ids := []string{string(a.addUser("one", true, false)), string(a.addUser("two", true, false))}
	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Go(func() {
			acc, _ := a.Account(ctx, userid.ID(id))
			a.h.Deleter.Delete(ctx, acc)
		})
	}
	wg.Wait()
	if n := a.count(`SELECT COUNT(*) FROM users WHERE is_admin = 1`); n != 1 {
		t.Fatalf("%d admins left", n)
	}
}
