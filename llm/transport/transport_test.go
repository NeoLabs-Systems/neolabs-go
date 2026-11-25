package transport

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestObjKeepsInsertionOrderAndWritesLikeJSONStringify(t *testing.T) {
	o := NewObj().Set("z", 1).Set("a", "<b>&")
	o.Set("z", 2) // existing keys keep their place
	o.Set("n", NewObj().Set("k", []any{1.5, nil, true}))
	got, err := Marshal(o)
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"z":2,"a":"<b>&","n":{"k":[1.5,null,true]}}`; string(got) != want {
		t.Errorf("Marshal = %s, want %s", got, want)
	}
	o.Delete("a")
	if got, _ := Marshal(o); string(got) != `{"z":2,"n":{"k":[1.5,null,true]}}` {
		t.Errorf("after delete: %s", got)
	}
	if got, _ := Marshal(FromMapTypeFirst(map[string]any{"b": 1, "type": "t", "a": 2})); string(got) != `{"type":"t","a":2,"b":1}` {
		t.Errorf("type first: %s", got)
	}
}

func TestParseOrderedRoundTripsKeyOrder(t *testing.T) {
	in := `{"b":1,"a":{"y":[3,{"q":null,"p":"x"}],"x":1.0},"c":"<"}`
	v, err := ParseOrdered([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	out, _ := Marshal(v)
	if want := `{"b":1,"a":{"y":[3,{"q":null,"p":"x"}],"x":1},"c":"<"}`; string(out) != want {
		t.Errorf("round trip = %s, want %s", out, want)
	}
	for _, bad := range []string{`{"a":1`, `{"a":1} x`, ``, `{"a" 1}`} {
		if _, err := ParseOrdered([]byte(bad)); err == nil {
			t.Errorf("%q should not parse", bad)
		}
	}
}

func TestReadSSE(t *testing.T) {
	stream := ": comment\r\nevent: a\r\ndata: one\r\n\r\ndata: x\ndata: y\n\n\ndata:no-space\n\nevent: only\n\ndata: tail"
	var got []SSEEvent
	if err := ReadSSE(strings.NewReader(stream), func(e SSEEvent) error { got = append(got, e); return nil }); err != nil {
		t.Fatal(err)
	}
	want := []SSEEvent{{"a", "one"}, {"", "x\ny"}, {"", "no-space"}, {"only", ""}, {"", "tail"}}
	if len(got) != len(want) {
		t.Fatalf("events = %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("event %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	stop := io.ErrClosedPipe
	err := ReadSSE(strings.NewReader("data: 1\n\ndata: 2\n\n"), func(SSEEvent) error { return stop })
	if err != stop {
		t.Errorf("callback error not returned: %v", err)
	}
	// a data line longer than the reader's buffer is joined, not cut
	long := strings.Repeat("x", 200_000)
	var n int
	_ = ReadSSE(strings.NewReader("data: "+long+"\n\n"), func(e SSEEvent) error { n = len(e.Data); return nil })
	if n != len(long) {
		t.Errorf("long line = %d bytes", n)
	}
}

func TestFetchTextLimitsAndDeadlines(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/big":
			io.WriteString(w, strings.Repeat("x", 100))
		case "/slow":
			time.Sleep(300 * time.Millisecond)
		default:
			w.WriteHeader(418)
			io.WriteString(w, "tea")
		}
	}))
	defer srv.Close()
	hc := srv.Client()
	res, err := FetchText(context.Background(), hc, FetchOptions{URL: srv.URL + "/ok", Provider: "p"})
	if err != nil || res.Status != 418 || res.Text != "tea" || res.OK() {
		t.Errorf("res = %+v, %v", res, err)
	}
	_, err = FetchText(context.Background(), hc, FetchOptions{URL: srv.URL + "/big", MaxBytes: 10, Service: "Thing"})
	if err == nil || err.Error() != "Thing response exceeded the 10-byte safety limit." {
		t.Errorf("too large: %v", err)
	}
	_, err = FetchText(context.Background(), hc, FetchOptions{URL: srv.URL + "/slow", Timeout: 40 * time.Millisecond, Service: "Thing"})
	if err == nil || err.Error() != "Thing request timed out after 40ms." {
		t.Errorf("deadline: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = FetchText(ctx, hc, FetchOptions{URL: srv.URL + "/ok"}); err != context.Canceled {
		t.Errorf("cancelled: %v", err)
	}
}

func TestFlightRunsOnceForConcurrentCallers(t *testing.T) {
	var f Flight[int]
	var runs atomic.Int32
	release := make(chan struct{})
	results := make(chan int, 3)
	for i := 0; i < 3; i++ {
		go func() {
			v, _ := f.Do(context.Background(), "k", func() (int, error) { runs.Add(1); <-release; return 7, nil })
			results <- v
		}()
	}
	time.Sleep(30 * time.Millisecond)
	close(release)
	for i := 0; i < 3; i++ {
		if v := <-results; v != 7 {
			t.Errorf("result = %d", v)
		}
	}
	if runs.Load() != 1 {
		t.Errorf("ran %d times", runs.Load())
	}
	// a waiter gives up when its context ends, the call carries on
	block := make(chan struct{})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := f.Do(ctx, "k2", func() (int, error) { <-block; return 1, nil }); err != context.DeadlineExceeded {
		t.Errorf("waiter: %v", err)
	}
	close(block)
}
