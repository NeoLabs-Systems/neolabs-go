package llm

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"
)

type redirectTo struct{ srv *httptest.Server }

func (r redirectTo) RoundTrip(req *http.Request) (*http.Response, error) {
	u, _ := url.Parse(r.srv.URL)
	req = req.Clone(req.Context())
	req.URL.Scheme, req.URL.Host = u.Scheme, u.Host
	return http.DefaultTransport.RoundTrip(req)
}

func TestReferenceLoadsOnceAndServesStaleWhileRefreshing(t *testing.T) {
	var loads atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		loads.Add(1)
		time.Sleep(30 * time.Millisecond)
		io.WriteString(w, `{"data":[{"id":"openai/gpt-5","pricing":{"prompt":"0.000001"}}]}`)
	}))
	defer srv.Close()
	hc := &http.Client{Transport: redirectTo{srv}}
	ref := NewReference()
	// concurrent first callers all wait for the one load
	done := make(chan bool, 3)
	for i := 0; i < 3; i++ {
		go func() {
			_ = ref.EnsureFresh(context.Background(), hc)
			_, ok := ref.Lookup("gpt-5")
			done <- ok
		}()
	}
	for i := 0; i < 3; i++ {
		if !<-done {
			t.Error("a first caller returned before the catalog loaded")
		}
	}
	if err := ref.EnsureFresh(context.Background(), hc); err != nil || loads.Load() != 1 || ref.Err() != nil {
		t.Errorf("fresh catalog reloaded: loads=%d err=%v", loads.Load(), ref.Err())
	}
	if tier := ref.PriceTier("gpt-5"); tier != "medium" {
		t.Errorf("tier = %q", tier)
	}
	// stale: served immediately, refreshed in the background
	ref.MarkLoaded(time.Now().Add(-7*time.Hour), true)
	start := time.Now()
	_ = ref.EnsureFresh(context.Background(), hc)
	if time.Since(start) > 20*time.Millisecond {
		t.Error("a stale refresh blocked the caller")
	}
	deadline := time.Now().Add(2 * time.Second)
	for loads.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if loads.Load() != 2 {
		t.Errorf("loads = %d", loads.Load())
	}
}

func TestReferenceFailureBacksOffFiveMinutes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer srv.Close()
	ref := NewReference()
	_ = ref.EnsureFresh(context.Background(), &http.Client{Transport: redirectTo{srv}})
	if ref.Err() == nil || ref.Err().Error() != "HTTP 503" {
		t.Errorf("err = %v", ref.Err())
	}
	if ref.Expired(time.Now().Add(4 * time.Minute)) {
		t.Error("retry too early")
	}
	if !ref.Expired(time.Now().Add(6 * time.Minute)) {
		t.Error("never retried")
	}
}
