package httpx

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCompressJSONOnlyLargeJSON(t *testing.T) {
	large := strings.Repeat("a", 4096)
	for _, c := range []struct {
		contentType, accept, body, encoding string
		status                              int
	}{
		{"application/json; charset=utf-8", "gzip, br", large, "gzip", 201},
		{"application/json", "gzip", "{}", "", 200},
		{"application/json", "", large, "", 200},
		{"application/octet-stream", "gzip", large, "", 206},
		{"application/json", "gzip", "", "", 204},
	} {
		h := CompressJSON()(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", c.contentType)
			w.WriteHeader(c.status)
			if c.body != "" {
				_, _ = io.WriteString(w, c.body)
			}
		}))
		r := httptest.NewRequest("GET", "/", nil)
		r.Header.Set("Accept-Encoding", c.accept)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if rec.Code != c.status || rec.Header().Get("Content-Encoding") != c.encoding {
			t.Fatalf("%+v: status %d encoding %q", c.status, rec.Code, rec.Header().Get("Content-Encoding"))
		}
		var body io.Reader = rec.Body
		if c.encoding == "gzip" {
			zr, err := gzip.NewReader(body)
			if err != nil {
				t.Fatal(err)
			}
			body = zr
		}
		got, err := io.ReadAll(body)
		if err != nil || string(got) != c.body {
			t.Fatalf("%d: body %d bytes, err %v", c.status, len(got), err)
		}
	}
}
