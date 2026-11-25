package wsgate

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/NeoLabs-Systems/neolabs-go/sessionx"
	"github.com/coder/websocket"
	_ "modernc.org/sqlite"
)

// fixture serves /login (starts a session for user 7) and /ws (an echo
// gateway behind the gate) behind the real session middleware.
type fixture struct {
	*httptest.Server
	gate   *Gate
	cookie string
}

func newFixture(t *testing.T, opts Options) *fixture {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	store, err := sessionx.NewStore(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	mgr, err := sessionx.NewManager(store, sessionx.Options{Name: "neoagent.sid", Secrets: []string{"s3cret"}})
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{gate: New(opts)}
	mux := http.NewServeMux()
	mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
		sessionx.FromContext(r.Context()).Set("userId", 7)
	})
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		userID, ok := f.gate.Admit(w, r)
		if !ok {
			return
		}
		c, err := f.gate.Accept(w, r)
		if err != nil {
			return
		}
		defer c.Terminate()
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		go c.KeepAlive(ctx, 20*time.Millisecond)
		_ = c.SendJSON(map[string]any{"hello": userID, "html": "<b>&"})
		for {
			bin, data, err := c.Read(ctx)
			if err != nil {
				return
			}
			if bin {
				_ = c.SendBinary(data)
			} else {
				_ = c.SendText(data)
			}
		}
	})
	f.Server = httptest.NewServer(mgr.Middleware(mux))
	t.Cleanup(f.Server.Close)
	res, err := http.Get(f.URL + "/login")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	for _, c := range res.Cookies() {
		f.cookie = c.Name + "=" + c.Value
	}
	if f.cookie == "" {
		t.Fatal("no session cookie")
	}
	return f
}

func (f *fixture) dial(t *testing.T, header http.Header) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	url := "ws" + strings.TrimPrefix(f.URL, "http") + "/ws"
	return websocket.Dial(context.Background(), url, &websocket.DialOptions{HTTPHeader: header})
}

func (f *fixture) signedIn() http.Header { return http.Header{"Cookie": {f.cookie}} }

func status(res *http.Response) int {
	if res == nil {
		return 0
	}
	return res.StatusCode
}

func TestAdmitsSignedInSessionsOnly(t *testing.T) {
	f := newFixture(t, Options{})
	_, res, err := f.dial(t, nil)
	if err == nil || status(res) != http.StatusUnauthorized {
		t.Fatalf("no cookie: %v %d", err, status(res))
	}
	_, res, err = f.dial(t, http.Header{"Cookie": {"neoagent.sid=" + "s%3Aforged.sig"}})
	if err == nil || status(res) != http.StatusUnauthorized {
		t.Fatalf("forged cookie: %v %d", err, status(res))
	}
	ws, _, err := f.dial(t, f.signedIn())
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()
	_, hello, err := ws.Read(context.Background())
	if err != nil || string(hello) != `{"hello":7,"html":"<b>&"}` {
		t.Fatalf("%q %v", hello, err)
	}
}

func TestEchoesTextAndBinary(t *testing.T) {
	f := newFixture(t, Options{})
	ws, _, err := f.dial(t, f.signedIn())
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()
	ws.Read(context.Background())
	ctx := context.Background()
	_ = ws.Write(ctx, websocket.MessageText, []byte("abc"))
	typ, data, _ := ws.Read(ctx)
	if typ != websocket.MessageText || string(data) != "abc" {
		t.Fatalf("text: %v %q", typ, data)
	}
	_ = ws.Write(ctx, websocket.MessageBinary, []byte{1, 2})
	typ, data, _ = ws.Read(ctx)
	if typ != websocket.MessageBinary || string(data) != "\x01\x02" {
		t.Fatalf("binary: %v %v", typ, data)
	}
}

func TestOriginPolicy(t *testing.T) {
	f := newFixture(t, Options{})
	h := f.signedIn()
	h.Set("Origin", "https://evil.example")
	if _, res, err := f.dial(t, h); err == nil || status(res) != http.StatusForbidden {
		t.Fatalf("foreign origin: %v %d", err, status(res))
	}
	h.Set("Origin", f.URL) // same host
	ws, _, err := f.dial(t, h)
	if err != nil {
		t.Fatalf("same-host origin: %v", err)
	}
	ws.CloseNow()

	allowed := newFixture(t, Options{AllowOrigin: func(o string) bool { return o == "https://app.example" }})
	h = allowed.signedIn()
	h.Set("Origin", "https://app.example")
	ws, _, err = allowed.dial(t, h)
	if err != nil {
		t.Fatalf("configured origin: %v", err)
	}
	ws.CloseNow()
	if _, res, err := allowed.dial(t, allowed.signedIn()); err == nil || status(res) != http.StatusForbidden {
		t.Fatalf("a policy that does not name the empty origin refuses native clients: %v %d", err, status(res))
	}
}

func TestUpgradeAttemptsAreLimitedPerAddress(t *testing.T) {
	f := newFixture(t, Options{UpgradeMax: 3})
	for i := 0; i < 3; i++ {
		ws, _, err := f.dial(t, f.signedIn())
		if err != nil {
			t.Fatalf("attempt %d: %v", i, err)
		}
		ws.CloseNow()
	}
	if _, res, err := f.dial(t, f.signedIn()); err == nil || status(res) != http.StatusTooManyRequests {
		t.Fatalf("over the limit: %v %d", err, status(res))
	}
}

func TestOversizedMessagesEndTheConnection(t *testing.T) {
	f := newFixture(t, Options{MaxMessage: 512})
	ws, _, err := f.dial(t, f.signedIn())
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()
	ws.Read(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = ws.Write(ctx, websocket.MessageBinary, make([]byte, 4096))
	if _, _, err := ws.Read(ctx); err == nil || websocket.CloseStatus(err) != websocket.StatusMessageTooBig {
		t.Fatalf("expected close 1009, got %v", err)
	}
}

func TestKeepAliveKeepsAnActiveClientConnected(t *testing.T) {
	f := newFixture(t, Options{})
	ws, _, err := f.dial(t, f.signedIn())
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	ws.Read(ctx) // hello; reading also answers the server's pings
	if _, _, err := ws.Read(ctx); err != nil && ctx.Err() == nil {
		t.Fatalf("connection dropped during keepalive: %v", err)
	}
}

func TestCloseDropsConnectionsAndRefusesNewOnes(t *testing.T) {
	f := newFixture(t, Options{})
	ws, _, err := f.dial(t, f.signedIn())
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()
	ws.Read(context.Background())
	f.gate.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, _, err := ws.Read(ctx); err == nil || ctx.Err() != nil {
		t.Fatalf("connection survived Close: %v", err)
	}
	if _, res, err := f.dial(t, f.signedIn()); err == nil || status(res) != http.StatusServiceUnavailable {
		t.Fatalf("after Close: %v %d", err, status(res))
	}
}

func TestSendAfterCloseFails(t *testing.T) {
	g := New(Options{})
	rec := make(chan error, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := g.Accept(w, r)
		if err != nil {
			rec <- err
			return
		}
		c.Terminate()
		rec <- c.SendText([]byte("x"))
	}))
	defer srv.Close()
	ws, _, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err == nil {
		defer ws.CloseNow()
	}
	if got := <-rec; got != ErrClosed {
		t.Fatalf("got %v", got)
	}
}
