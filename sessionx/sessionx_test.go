package sessionx

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func TestSignatureMatchesNodeCookieSignature(t *testing.T) {
	raw, err := os.ReadFile("testdata/vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Secret string
		Cases  []struct{ Sid, Signed, Header string }
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	for _, c := range v.Cases {
		if got := "s:" + Sign(c.Sid, v.Secret); got != c.Signed {
			t.Fatalf("sign %s: got %q want %q", c.Sid, got, c.Signed)
		}
		header := "neoagent.sid=" + EncodeCookieValue(c.Sid, v.Secret)
		if !strings.HasPrefix(c.Header, header) {
			t.Fatalf("cookie %q does not start with %q", c.Header, header)
		}
		if sid, ok := DecodeCookieValue(EncodeCookieValue(c.Sid, v.Secret), v.Secret); !ok || sid != c.Sid {
			t.Fatalf("decode %s: %q %v", c.Sid, sid, ok)
		}
	}
}

func TestUnsignRejectsTamperingAndAllowsRotation(t *testing.T) {
	v := EncodeCookieValue("sid1", "old")
	if _, ok := DecodeCookieValue(v, "new"); ok {
		t.Fatal("wrong secret accepted")
	}
	if sid, ok := DecodeCookieValue(v, "new", "old"); !ok || sid != "sid1" {
		t.Fatal("rotation secret rejected")
	}
	if _, ok := DecodeCookieValue(strings.Replace(v, "sid1", "sid2", 1), "old"); ok {
		t.Fatal("tampered id accepted")
	}
	for _, bad := range []string{"", "s%3Aabc", "abc.def", "%zz"} {
		if _, ok := DecodeCookieValue(bad, "old"); ok {
			t.Fatalf("accepted %q", bad)
		}
	}
}

func newTestManager(t *testing.T, opt Options) (*Manager, *Store) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+t.TempDir()+"/s.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	store, err := NewStore(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	opt.Name, opt.Secrets = "neoagent.sid", []string{"secret"}
	m, err := NewManager(store, opt)
	if err != nil {
		t.Fatal(err)
	}
	return m, store
}

func TestStoreReadsNodeFormatRowsAndExpires(t *testing.T) {
	_, store := newTestManager(t, Options{})
	ctx := context.Background()
	future := time.Now().Add(time.Hour).UTC().Format("2006-01-02T15:04:05.000Z")
	past := time.Now().Add(-time.Hour).UTC().Format("2006-01-02T15:04:05.000Z")
	node := `{"cookie":{"originalMaxAge":2592000000,"expires":"` + future + `","httpOnly":true,"path":"/","sameSite":"lax"},"userId":12345678901234567}`
	for sid, exp := range map[string]string{"live": future, "dead": past} {
		if _, err := store.db.Exec(`INSERT INTO sessions (sid, sess, expire) VALUES (?,?,?)`, sid, node, exp); err != nil {
			t.Fatal(err)
		}
	}
	got, err := store.Get(ctx, "live")
	if err != nil || got == nil {
		t.Fatalf("live: %v %v", got, err)
	}
	if got["userId"].(json.Number).String() != "12345678901234567" {
		t.Fatalf("userId lost precision: %v", got["userId"])
	}
	if dead, _ := store.Get(ctx, "dead"); dead != nil {
		t.Fatal("expired session returned")
	}
}

func serve(m *Manager, h http.HandlerFunc, cookie *http.Cookie, tls bool, proto string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", "http://example.test/", nil)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	if tls {
		req = httptest.NewRequest("GET", "https://example.test/", nil)
		if cookie != nil {
			req.AddCookie(cookie)
		}
	}
	if proto != "" {
		req.Header.Set("X-Forwarded-Proto", proto)
	}
	rec := httptest.NewRecorder()
	m.Middleware(h).ServeHTTP(rec, req)
	return rec
}

func TestSessionLifecycle(t *testing.T) {
	m, _ := newTestManager(t, Options{Rolling: true})

	// Untouched session: no cookie, no row (saveUninitialized=false).
	rec := serve(m, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }, nil, false, "")
	if len(rec.Result().Cookies()) != 0 {
		t.Fatal("empty session must not set a cookie")
	}

	// Login: data set → cookie with the expected attributes.
	rec = serve(m, func(w http.ResponseWriter, r *http.Request) {
		FromContext(r.Context()).Set("userId", json.Number("42"))
		w.WriteHeader(204)
	}, nil, false, "")
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("cookies: %v", cookies)
	}
	c := cookies[0]
	if !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.Path != "/" || c.Expires.IsZero() {
		t.Fatalf("cookie attrs: %+v", c)
	}

	// Next request sees the data and rolls the cookie.
	var seen string
	rec = serve(m, func(w http.ResponseWriter, r *http.Request) {
		seen = FromContext(r.Context()).String("userId")
	}, c, false, "")
	if seen != "42" || len(rec.Result().Cookies()) != 1 {
		t.Fatalf("seen=%q cookies=%v", seen, rec.Result().Cookies())
	}

	// Tampered cookie → fresh anonymous session.
	bad := &http.Cookie{Name: c.Name, Value: c.Value + "x"}
	serve(m, func(w http.ResponseWriter, r *http.Request) { seen = FromContext(r.Context()).String("userId") }, bad, false, "")
	if seen != "" {
		t.Fatal("tampered cookie loaded a session")
	}

	// Destroy removes the row.
	serve(m, func(w http.ResponseWriter, r *http.Request) { FromContext(r.Context()).Destroy() }, c, false, "")
	serve(m, func(w http.ResponseWriter, r *http.Request) { seen = FromContext(r.Context()).String("userId") }, c, false, "")
	if seen != "" {
		t.Fatal("destroyed session still loads")
	}
}

func TestRegenerateClearsDataAndRotateKeepsIt(t *testing.T) {
	m, _ := newTestManager(t, Options{})
	first := serve(m, func(w http.ResponseWriter, r *http.Request) { FromContext(r.Context()).Set("pending", "x") }, nil, false, "").Result().Cookies()[0]
	var after string
	rec := serve(m, func(w http.ResponseWriter, r *http.Request) {
		s := FromContext(r.Context())
		s.Regenerate()
		s.Set("userId", int64(1))
		after = s.String("pending")
	}, first, false, "")
	if after != "" {
		t.Fatal("Regenerate must drop pre-login state")
	}
	next := rec.Result().Cookies()[0]
	serve(m, func(w http.ResponseWriter, r *http.Request) { FromContext(r.Context()).Rotate() }, next, false, "")
	var uid int64
	serve(m, func(w http.ResponseWriter, r *http.Request) { uid, _ = FromContext(r.Context()).Int64("userId") }, next, false, "")
	if uid != 0 {
		t.Fatal("rotating must retire the old id")
	}
}

func TestRegenerateReplacesId(t *testing.T) {
	m, store := newTestManager(t, Options{})
	rec := serve(m, func(w http.ResponseWriter, r *http.Request) { FromContext(r.Context()).Set("a", "1") }, nil, false, "")
	first := rec.Result().Cookies()[0]
	rec = serve(m, func(w http.ResponseWriter, r *http.Request) { FromContext(r.Context()).Rotate() }, first, false, "")
	second := rec.Result().Cookies()[0]
	if first.Value == second.Value {
		t.Fatal("id not regenerated")
	}
	oldID, _ := DecodeCookieValue(first.Value, "secret")
	if data, _ := store.Get(context.Background(), oldID); data != nil {
		t.Fatal("old session row survived regeneration")
	}
	var a string
	serve(m, func(w http.ResponseWriter, r *http.Request) { a = FromContext(r.Context()).String("a") }, second, false, "")
	if a != "1" {
		t.Fatal("data lost across regeneration")
	}
}

func TestSecureCookieNeedsSecureTransport(t *testing.T) {
	set := func(w http.ResponseWriter, r *http.Request) { FromContext(r.Context()).Set("k", "v") }

	m, _ := newTestManager(t, Options{Secure: true})
	if rec := serve(m, set, nil, false, ""); len(rec.Result().Cookies()) != 0 {
		t.Fatal("secure cookie set over plain http")
	}
	if rec := serve(m, set, nil, false, "https"); len(rec.Result().Cookies()) != 0 {
		t.Fatal("X-Forwarded-Proto trusted without TrustProxy")
	}
	if rec := serve(m, set, nil, true, ""); len(rec.Result().Cookies()) != 1 || !rec.Result().Cookies()[0].Secure {
		t.Fatal("secure cookie missing over TLS")
	}
	mp, _ := newTestManager(t, Options{Secure: true, TrustProxy: true})
	if rec := serve(mp, set, nil, false, "https"); len(rec.Result().Cookies()) != 1 {
		t.Fatal("proxy-terminated TLS should set the cookie")
	}
}

func TestDestroyForUser(t *testing.T) {
	m, store := newTestManager(t, Options{})
	login := func(id string) *http.Cookie {
		rec := serve(m, func(w http.ResponseWriter, r *http.Request) { FromContext(r.Context()).Set("userId", json.Number(id)) }, nil, false, "")
		return rec.Result().Cookies()[0]
	}
	a1, a2, b := login("1"), login("1"), login("2")
	if err := store.DestroyForUser(context.Background(), "1"); err != nil {
		t.Fatal(err)
	}
	for _, c := range []*http.Cookie{a1, a2} {
		sid, _ := DecodeCookieValue(c.Value, "secret")
		if d, _ := store.Get(context.Background(), sid); d != nil {
			t.Fatal("user 1 session survived")
		}
	}
	sid, _ := DecodeCookieValue(b.Value, "secret")
	if d, _ := store.Get(context.Background(), sid); d == nil {
		t.Fatal("user 2 session was removed")
	}
}

func TestHashIDMatchesNodeHmac(t *testing.T) {
	// crypto.createHmac('sha256', 'secret').update('sid-1').digest('hex')
	const want = "128214795fd96fda007331ea9fc6cd7bab6134bf147d0d00f6bbf06af2670fcd"
	if got := HashID("secret", "sid-1"); got != want {
		t.Fatalf("got %s", got)
	}
}

func TestSessionIDsPerUserMaxAgeAndHelpers(t *testing.T) {
	m, store := newTestManager(t, Options{})
	var cookies []*http.Cookie
	for _, id := range []string{"5", "5", "6"} {
		rec := serve(m, func(w http.ResponseWriter, r *http.Request) {
			s := FromContext(r.Context())
			n, _ := strconv.Atoi(id)
			s.Set("userId", int64(n))
			if id == "6" {
				s.SetMaxAge(90 * 24 * time.Hour)
			}
		}, nil, false, "")
		cookies = append(cookies, rec.Result().Cookies()[0])
	}
	ids, err := store.IDsForUser(context.Background(), "5")
	if err != nil || len(ids) != 2 {
		t.Fatalf("ids %v %v", ids, err)
	}
	if long := cookies[2].Expires; time.Until(long) < 80*24*time.Hour {
		t.Fatalf("per-session lifetime ignored: %v", long)
	}
	var sid, uid string
	serve(m, func(w http.ResponseWriter, r *http.Request) {
		s := FromContext(r.Context())
		sid, uid = s.ID(), s.String("userId")
	}, cookies[0], false, "")
	if sid == "" || uid != "5" {
		t.Fatalf("ID()=%q userId=%q (numbers must stringify)", sid, uid)
	}
}

func TestForUserListsLiveSessionsWithData(t *testing.T) {
	_, store := newTestManager(t, Options{})
	ctx := context.Background()
	store.Set(ctx, "a", map[string]any{"userId": 7, "userAgent": "phone"}, time.Hour)
	store.Set(ctx, "b", map[string]any{"userId": "7"}, 2*time.Hour)
	store.Set(ctx, "c", map[string]any{"userId": 8}, time.Hour)
	store.Set(ctx, "d", map[string]any{"userId": 7}, -time.Hour)
	rows, err := store.ForUser(ctx, "7")
	if err != nil || len(rows) != 2 {
		t.Fatalf("%+v %v", rows, err)
	}
	if rows[0].SID != "b" || rows[1].SID != "a" || rows[1].Data["userAgent"] != "phone" {
		t.Fatalf("order or data: %+v", rows)
	}
	if time.Until(rows[0].Expire) < time.Hour {
		t.Fatalf("expiry not read: %v", rows[0].Expire)
	}
}
