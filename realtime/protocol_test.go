package realtime

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// rawClient speaks Engine.IO by hand over a websocket, so the tests see the
// exact frames a server sends.
type rawClient struct {
	t  *testing.T
	ws *websocket.Conn
}

func dialRaw(t *testing.T, ts *testServer, header http.Header) *rawClient {
	t.Helper()
	url := "ws" + strings.TrimPrefix(ts.URL, "http") + "/socket.io/?EIO=4&transport=websocket"
	ws, _, err := websocket.Dial(context.Background(), url, &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ws.CloseNow() })
	return &rawClient{t: t, ws: ws}
}

func (c *rawClient) read(timeout time.Duration) (websocket.MessageType, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	typ, data, err := c.ws.Read(ctx)
	return typ, string(data), err
}

func (c *rawClient) expect(want string) {
	c.t.Helper()
	_, got, err := c.read(3 * time.Second)
	if err != nil {
		c.t.Fatalf("waiting for %q: %v", want, err)
	}
	if got != want {
		c.t.Fatalf("got %q want %q", got, want)
	}
}

func (c *rawClient) send(s string) {
	c.t.Helper()
	if err := c.ws.Write(context.Background(), websocket.MessageText, []byte(s)); err != nil {
		c.t.Fatal(err)
	}
}

func TestWebsocketHandshakeAndEventFlow(t *testing.T) {
	ts := newTestServer(t, Options{PingInterval: 40 * time.Second})
	c := dialRaw(t, ts, nil)
	_, open, err := c.read(3 * time.Second)
	if err != nil || !strings.HasPrefix(open, `0{"sid":"`) ||
		!strings.HasSuffix(open, `","upgrades":[],"pingInterval":40000,"pingTimeout":20000,"maxPayload":1000000}`) {
		t.Fatalf("open packet %q %v", open, err)
	}
	c.send(`40{"token":"secret:3"}`)
	_, connected, _ := c.read(3 * time.Second)
	if !strings.HasPrefix(connected, `40{"sid":"`) {
		t.Fatalf("connect reply %q", connected)
	}
	c.send(`421["echo",{"a":1},"x"]`)
	c.expect(`431[{"a":1},"x"]`)
	// An unknown event is ignored, a known one still answers.
	c.send(`42["unknown"]`)
	c.send(`422["echo"]`)
	c.expect(`432[]`)
	// Server pings are answered by the client with a pong, and the client may ping too.
	c.send("2")
	c.expect("3")
}

func TestConnectErrors(t *testing.T) {
	ts := newTestServer(t, Options{})
	c := dialRaw(t, ts, nil)
	c.read(time.Second)
	c.send(`40{"token":"wrong"}`)
	c.expect(`44{"message":"unauthorized"}`)
	c.send(`40/missing,`)
	c.expect(`44/missing,{"message":"Invalid namespace"}`)
	c.send(`40/admin,{"token":"secret:1"}`)
	_, got, _ := c.read(time.Second)
	if !strings.HasPrefix(got, `40/admin,{"sid":"`) {
		t.Fatalf("%q", got)
	}
	c.send(`42/admin,7["hello"]`)
	c.expect(`43/admin,7["admin"]`)
}

func TestPingTimeoutClosesTheSocket(t *testing.T) {
	ts := newTestServer(t, Options{PingInterval: 50 * time.Millisecond, PingTimeout: 100 * time.Millisecond})
	c := dialRaw(t, ts, nil)
	c.read(time.Second)
	c.send(`40{"token":"secret:3"}`)
	c.read(time.Second)
	c.expect("2") // the server pings; this client stays silent
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if r := ts.reasons(); len(r) == 1 {
			if r[0] != "ping timeout" {
				t.Fatalf("reason %q", r[0])
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("socket was not closed")
}

func TestAnsweredPingsKeepTheSocketOpen(t *testing.T) {
	ts := newTestServer(t, Options{PingInterval: 30 * time.Millisecond, PingTimeout: 100 * time.Millisecond})
	c := dialRaw(t, ts, nil)
	c.read(time.Second)
	c.send(`40{"token":"secret:3"}`)
	c.read(time.Second)
	for i := 0; i < 6; i++ {
		c.expect("2")
		c.send("3")
	}
	if len(ts.reasons()) != 0 {
		t.Fatalf("closed early: %v", ts.reasons())
	}
}

func TestConnectTimeoutClosesIdleConnections(t *testing.T) {
	ts := newTestServer(t, Options{ConnectTimeout: 80 * time.Millisecond})
	c := dialRaw(t, ts, nil)
	c.read(time.Second)
	if _, _, err := c.read(2 * time.Second); err == nil {
		t.Fatal("connection stayed open without joining a namespace")
	}
}

func TestOriginPolicy(t *testing.T) {
	allow := func(origin string) bool { return origin == "" || origin == "https://app.example" }
	ts := newTestServer(t, Options{AllowOrigin: allow})
	get := func(origin string) *http.Response {
		req, _ := http.NewRequest(http.MethodGet, ts.URL+"/socket.io/?EIO=4&transport=polling", nil)
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { res.Body.Close() })
		return res
	}
	if res := get("https://evil.example"); res.StatusCode != 403 {
		t.Fatalf("foreign origin: %d", res.StatusCode)
	}
	res := get("https://app.example")
	if res.StatusCode != 200 || res.Header.Get("Access-Control-Allow-Origin") != "https://app.example" ||
		res.Header.Get("Access-Control-Allow-Credentials") != "true" {
		t.Fatalf("allowed origin: %d %v", res.StatusCode, res.Header)
	}
	if res := get(""); res.StatusCode != 200 {
		t.Fatalf("no origin: %d", res.StatusCode)
	}
	pre, _ := http.NewRequest(http.MethodOptions, ts.URL+"/socket.io/?EIO=4&transport=polling", nil)
	pre.Header.Set("Origin", "https://app.example")
	pre.Header.Set("Access-Control-Request-Method", "POST")
	pre.Header.Set("Access-Control-Request-Headers", "content-type")
	pres, err := http.DefaultClient.Do(pre)
	if err != nil {
		t.Fatal(err)
	}
	pres.Body.Close()
	if pres.StatusCode != 204 || pres.Header.Get("Access-Control-Allow-Headers") != "content-type" {
		t.Fatalf("preflight: %d %v", pres.StatusCode, pres.Header)
	}
	// A websocket upgrade from a foreign origin is refused as well.
	url := "ws" + strings.TrimPrefix(ts.URL, "http") + "/socket.io/?EIO=4&transport=websocket"
	_, _, err = websocket.Dial(context.Background(), url, &websocket.DialOptions{HTTPHeader: http.Header{"Origin": {"https://evil.example"}}})
	if err == nil {
		t.Fatal("websocket from a foreign origin was accepted")
	}
}

func TestDefaultOriginPolicyIsSameHost(t *testing.T) {
	ts := newTestServer(t, Options{})
	host := strings.TrimPrefix(ts.URL, "http://")
	for origin, want := range map[string]int{"http://" + host: 200, "http://other.example": 403} {
		req, _ := http.NewRequest(http.MethodGet, ts.URL+"/socket.io/?EIO=4&transport=polling", nil)
		req.Header.Set("Origin", origin)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != want {
			t.Errorf("%s: %d want %d", origin, res.StatusCode, want)
		}
	}
}

func TestEngineErrors(t *testing.T) {
	ts := newTestServer(t, Options{})
	for _, c := range []struct {
		method, query string
		status        int
		body          string
	}{
		{"GET", "EIO=3&transport=polling", 400, `{"code":5,"message":"Unsupported protocol version"}`},
		{"GET", "EIO=4&transport=carrier", 400, `{"code":0,"message":"Transport unknown"}`},
		{"GET", "EIO=4&transport=polling&sid=nope", 400, `{"code":1,"message":"Session ID unknown"}`},
		{"POST", "EIO=4&transport=polling", 400, `{"code":2,"message":"Bad handshake method"}`},
	} {
		req, _ := http.NewRequest(c.method, ts.URL+"/socket.io/?"+c.query, strings.NewReader("x"))
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != c.status || string(body) != c.body {
			t.Errorf("%s %s: %d %s", c.method, c.query, res.StatusCode, body)
		}
	}
}

func TestMalformedPacketsCloseTheConnection(t *testing.T) {
	for _, bad := range []string{"9", "4", "4zzz", "42[1]", "42"} {
		ts := newTestServer(t, Options{})
		c := dialRaw(t, ts, nil)
		c.read(time.Second)
		c.send(`40{"token":"secret:3"}`)
		c.read(time.Second)
		c.send(bad)
		if _, _, err := c.read(2 * time.Second); err == nil {
			t.Errorf("%q: connection stayed open", bad)
		}
	}
}

func TestOversizedMessageClosesTheConnection(t *testing.T) {
	ts := newTestServer(t, Options{MaxPayload: 1024})
	c := dialRaw(t, ts, nil)
	c.read(time.Second)
	c.send(`40{"token":"secret:3"}`)
	c.read(time.Second)
	c.send(`42["echo","` + strings.Repeat("a", 4096) + `"]`)
	if _, _, err := c.read(2 * time.Second); err == nil {
		t.Fatal("oversized message was accepted")
	}
}

func TestSlowClientIsDisconnectedNotBuffered(t *testing.T) {
	ts := newTestServer(t, Options{MaxQueuedPackets: 16})
	c := dialRaw(t, ts, nil)
	c.read(time.Second)
	c.send(`40{"token":"secret:3"}`)
	c.read(time.Second)
	// Flood the room faster than this client reads; the hub must cut it off.
	big := strings.Repeat("x", 1<<16)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for i := 0; i < 64; i++ {
			ts.hub.EmitToUser(3, "flood", big)
		}
		for _, r := range ts.reasons() {
			if r == "buffer overflow" {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("slow client was never cut off: %v", ts.reasons())
}

func TestVolatileEventsAreDroppedForBackloggedClients(t *testing.T) {
	hub := New(Options{})
	c := &conn{h: hub, notify: make(chan struct{}, 1), sockets: map[string]*Socket{}, done: make(chan struct{})}
	pkt := []outPacket{{kind: eioMessage, text: "2[\"x\"]"}}
	for i := 0; i < 20; i++ {
		c.enqueue(pkt, true)
	}
	if len(c.queue) > volatileBacklog+1 {
		t.Fatalf("volatile packets piled up: %d", len(c.queue))
	}
	for i := 0; i < 20; i++ {
		c.enqueue(pkt, false)
	}
	if len(c.queue) < 20 {
		t.Fatalf("regular packets must not be dropped: %d", len(c.queue))
	}
}

func TestHubCloseDisconnectsClientsAndRefusesNewOnes(t *testing.T) {
	ts := newTestServer(t, Options{})
	c := dialRaw(t, ts, nil)
	c.read(time.Second)
	c.send(`40{"token":"secret:3"}`)
	c.read(time.Second)
	ts.hub.Close()
	if _, _, err := c.read(2 * time.Second); err == nil {
		t.Fatal("client still connected after Close")
	}
	res, err := http.Get(ts.URL + "/socket.io/?EIO=4&transport=polling")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("new connection after Close: %d", res.StatusCode)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(ts.reasons()) == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if r := ts.reasons(); len(r) != 1 || r[0] != "server shutting down" {
		t.Fatalf("reasons %v", r)
	}
}

func TestOverlappingPollsAreRejected(t *testing.T) {
	ts := newTestServer(t, Options{})
	res, err := http.Get(ts.URL + "/socket.io/?EIO=4&transport=polling")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	sid := strings.Split(strings.TrimPrefix(string(body), `0{"sid":"`), `"`)[0]
	url := ts.URL + "/socket.io/?EIO=4&transport=polling&sid=" + sid
	first := make(chan int, 1)
	go func() {
		r, err := http.Get(url)
		if err != nil {
			first <- 0
			return
		}
		r.Body.Close()
		first <- r.StatusCode
	}()
	time.Sleep(100 * time.Millisecond)
	second, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	second.Body.Close()
	if second.StatusCode != 400 {
		t.Fatalf("overlapping poll: %d", second.StatusCode)
	}
	<-first
}
