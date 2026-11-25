package wsclient

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func echoServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := Accept(w, r, AcceptOptions{ReadLimit: 1024})
		if err != nil {
			return
		}
		defer c.CloseNow()
		for {
			_, data, err := c.Read(r.Context())
			if err != nil {
				return
			}
			if string(data) == "bye" {
				_ = c.Close(CloseNormal, "done")
				return
			}
			if err := c.WriteText(r.Context(), data); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestEchoAndCloseReason(t *testing.T) {
	srv := echoServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := Dial(ctx, srv.URL, Options{Header: http.Header{"X-Test": {"1"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	if err := c.WriteJSON(ctx, map[string]string{"a": "<b>"}); err != nil {
		t.Fatal(err)
	}
	bin, data, err := c.Read(ctx)
	if err != nil || bin || string(data) != `{"a":"<b>"}` {
		t.Fatalf("echo = %q bin=%v err=%v", data, bin, err)
	}
	if err := c.WriteText(ctx, []byte("bye")); err != nil {
		t.Fatal(err)
	}
	_, _, err = c.Read(ctx)
	if CloseStatus(err) != CloseNormal || CloseReason(err) != "done" {
		t.Fatalf("close = %d %q (%v)", CloseStatus(err), CloseReason(err), err)
	}
}

func TestReadLimitEnforced(t *testing.T) {
	srv := echoServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := Dial(ctx, srv.URL, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	_ = c.WriteText(ctx, []byte(strings.Repeat("x", 4096)))
	if _, _, err := c.Read(ctx); err == nil {
		t.Fatal("server accepted a message over its read limit")
	}
}

func TestDialRefusedReportsStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "no", 401) }))
	defer srv.Close()
	_, err := Dial(context.Background(), srv.URL, Options{})
	if err == nil || !strings.Contains(err.Error(), "(401)") {
		t.Fatalf("err = %v", err)
	}
}

func TestBackoffDelay(t *testing.T) {
	b := Backoff{Min: time.Second, Max: 5 * time.Second, Factor: 2}
	for attempt, want := range map[int]time.Duration{1: time.Second, 2: 2 * time.Second, 3: 4 * time.Second, 4: 5 * time.Second, 9: 5 * time.Second} {
		if got := b.Delay(attempt); got != want {
			t.Errorf("Delay(%d) = %v, want %v", attempt, got, want)
		}
	}
	j := Backoff{Min: time.Second, Max: time.Minute, Factor: 2, Jitter: 0.5}
	for i := 0; i < 50; i++ {
		if d := j.Delay(1); d < time.Second || d > 1500*time.Millisecond {
			t.Fatalf("jittered delay %v out of range", d)
		}
	}
}

func TestReconnectRetriesThenGivesUp(t *testing.T) {
	var dials int32
	var slept []time.Duration
	r := Reconnect{
		Dial: func(context.Context) (*Conn, error) {
			atomic.AddInt32(&dials, 1)
			return nil, errors.New("refused")
		},
		Serve:       func(context.Context, *Conn) error { return nil },
		Backoff:     Backoff{Min: time.Second, Max: 8 * time.Second, Factor: 2},
		MaxAttempts: 4,
		Sleep:       func(_ context.Context, d time.Duration) error { slept = append(slept, d); return nil },
	}
	err := r.Run(context.Background())
	if !errors.Is(err, ErrGaveUp) || dials != 4 {
		t.Fatalf("err=%v dials=%d", err, dials)
	}
	if len(slept) != 3 || slept[0] != time.Second || slept[2] != 4*time.Second {
		t.Fatalf("slept = %v", slept)
	}
}

func TestReconnectServesAgainAfterDrop(t *testing.T) {
	srv := echoServer(t)
	var served int32
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r := Reconnect{
		Dial: func(ctx context.Context) (*Conn, error) { return Dial(ctx, srv.URL, Options{}) },
		Serve: func(ctx context.Context, c *Conn) error {
			if atomic.AddInt32(&served, 1) == 1 {
				return errors.New("dropped")
			}
			return nil
		},
		Backoff: Backoff{Min: time.Millisecond, Max: time.Millisecond},
	}
	if err := r.Run(ctx); err != nil || served != 2 {
		t.Fatalf("err=%v served=%d", err, served)
	}
}

func TestMarshalLikeJSONStringify(t *testing.T) {
	b, err := Marshal(map[string]any{"k": "a&b<c>"})
	if err != nil || string(b) != `{"k":"a&b<c>"}` {
		t.Fatalf("%q %v", b, err)
	}
}
