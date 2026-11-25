package oauthclient

import (
	"net/url"
	"testing"
)

func TestPinnedURL(t *testing.T) {
	for _, path := range []string{"https://evil.example/v1/x", "//evil.example/v1/x", "https://user:pass@api.example/v1/x"} {
		if _, err := PinnedURL("https://api.example", path, nil); err == nil {
			t.Fatalf("accepted %q", path)
		}
	}
	got, err := PinnedURL("https://api.example", "v1/x?keep=1", url.Values{"q": {"hello world"}, "empty": {""}})
	if err != nil || got != "https://api.example/v1/x?keep=1&q=hello+world" {
		t.Fatalf("%q %v", got, err)
	}
}
