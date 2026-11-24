package ssrf

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
)

func TestCheckAddr(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "::1", "10.1.2.3", "172.16.0.1", "172.31.255.255", "192.168.1.1",
		"169.254.169.254", "fe80::1", "fd00::1", "100.64.0.1", "0.0.0.0", "::",
		"224.0.0.1", "255.255.255.255", "198.18.0.1", "::ffff:127.0.0.1", "::ffff:10.0.0.1",
		"64:ff9b::7f00:1", "240.0.0.1",
	}
	for _, s := range blocked {
		if err := CheckAddr(netip.MustParseAddr(s)); !errors.Is(err, ErrBlocked) {
			t.Errorf("%s must be blocked, got %v", s, err)
		}
	}
	for _, s := range []string{"8.8.8.8", "1.1.1.1", "93.184.216.34", "2606:4700:4700::1111", "172.32.0.1"} {
		if err := CheckAddr(netip.MustParseAddr(s)); err != nil {
			t.Errorf("%s must be allowed: %v", s, err)
		}
	}
}

func TestCheckURL(t *testing.T) {
	for _, bad := range []string{"file:///etc/passwd", "gopher://x", "ftp://x", "http://user:pw@example.com", "http://", "::bad", "javascript:alert(1)"} {
		if _, err := CheckURL(bad); !errors.Is(err, ErrBlocked) {
			t.Errorf("%q should be rejected, got %v", bad, err)
		}
	}
	if _, err := CheckURL("https://example.com/a?b=c"); err != nil {
		t.Fatal(err)
	}
}

func TestClientRefusesLoopbackButCanBeOpened(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("hi")) }))
	defer srv.Close()

	if _, err := NewClient(Options{}).Get(srv.URL); err == nil || !errors.Is(err, ErrBlocked) {
		t.Fatalf("loopback must be blocked, got %v", err)
	}
	resp, err := NewClient(Options{AllowPrivate: true}).Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}

func TestRedirectToInternalIsRefused(t *testing.T) {
	c := NewClient(Options{})
	via := make([]*http.Request, 1)
	req := httptest.NewRequest("GET", "file:///etc/passwd", nil)
	if err := c.CheckRedirect(req, via); !errors.Is(err, ErrBlocked) {
		t.Fatalf("redirect to file: must be refused, got %v", err)
	}
	if err := c.CheckRedirect(httptest.NewRequest("GET", "https://example.com/", nil), make([]*http.Request, 6)); err == nil {
		t.Fatal("redirect limit not enforced")
	}
}
