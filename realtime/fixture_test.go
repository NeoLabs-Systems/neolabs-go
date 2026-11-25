package realtime

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// testServer is a Hub with a small event vocabulary that exercises every
// feature a client can use.
type testServer struct {
	*httptest.Server
	hub *Hub

	mu          sync.Mutex
	disconnects []string // reasons, in order
}

func (ts *testServer) reasons() []string {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return append([]string(nil), ts.disconnects...)
}

func newTestServer(t *testing.T, opts Options) *testServer {
	t.Helper()
	hub := New(opts)
	ts := &testServer{hub: hub}

	hub.Use(func(s *Socket, auth json.RawMessage) error {
		var a struct {
			Token string `json:"token"`
		}
		_ = json.Unmarshal(auth, &a)
		if !strings.HasPrefix(a.Token, "secret:") {
			return errors.New("unauthorized")
		}
		s.Data.Store("user", strings.TrimPrefix(a.Token, "secret:"))
		return nil
	})
	hub.OnConnection(func(s *Socket) {
		user, _ := s.Data.Load("user")
		uid, _ := strconv.ParseInt(user.(string), 10, 64)
		s.Join(UserRoom(uid))
		s.OnDisconnect(func(reason string) {
			ts.mu.Lock()
			ts.disconnects = append(ts.disconnects, reason)
			ts.mu.Unlock()
		})
		s.On("echo", func(e *Event) { e.Ack(e.Args...) })
		s.On("whoami", func(e *Event) {
			e.Ack(map[string]any{
				"user":   user,
				"cookie": s.Request().Header.Get("Cookie"),
				"id":     s.ID(),
				"rooms":  len(s.roomSet()),
			})
		})
		s.On("bin", func(e *Event) {
			raw, _ := e.Arg(0).([]byte)
			rev := make([]byte, len(raw))
			for i, b := range raw {
				rev[len(raw)-1-i] = b
			}
			_ = s.Emit("pushbin", map[string]any{"n": len(raw)}, rev)
			e.Ack(rev)
		})
		s.On("join", func(e *Event) { s.Join(e.Arg(0).(string)); e.Ack(true) })
		s.On("to-room", func(e *Event) {
			o := e.Object(0)
			_ = s.To(o["room"].(string)).Emit("room:msg", o["msg"])
			e.Ack(true)
		})
		s.On("hub-user", func(e *Event) {
			hub.EmitToUser(uid, "hub:user", map[string]any{"for": uid})
			hub.EmitToRoom("r1", "hub:room", nil)
			e.Ack(true)
		})
		s.On("ask", func(e *Event) {
			err := s.EmitWithAck("question", 2*time.Second, func(args []any, err error) {
				if err != nil {
					e.Ack("timeout")
					return
				}
				e.Ack(args...)
			}, e.Arg(0))
			if err != nil {
				e.Ack("error")
			}
		})
		s.On("ask-silent", func(e *Event) {
			_ = s.EmitWithAck("never", 150*time.Millisecond, func(args []any, err error) {
				e.Ack(errors.Is(err, ErrAckTimeout))
			})
		})
		s.On("bye", func(e *Event) { s.Disconnect(false) })
		s.On("bye-all", func(e *Event) { s.Disconnect(true) })
		s.On("drop", func(e *Event) { s.c.close("test drop") })
		s.On("slow", func(e *Event) { time.Sleep(300 * time.Millisecond); e.Ack("done") })
		s.On("order", func(e *Event) { _ = s.Emit("order", e.Arg(0)) })
	})
	admin := hub.Of("/admin")
	admin.OnConnection(func(s *Socket) {
		s.On("hello", func(e *Event) { e.Ack("admin") })
	})

	mux := http.NewServeMux()
	mux.Handle("/socket.io/", hub)
	ts.Server = httptest.NewServer(mux)
	t.Cleanup(func() {
		hub.Close()
		ts.Server.Close()
	})
	return ts
}
