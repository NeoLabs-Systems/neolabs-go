package publicerr

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"reflect"
	"strconv"
	"strings"
)

// Middleware redacts JSON responses of `/api/` routes for requests that are not
// from an administrator. Responses that are not JSON, and downloads (anything
// with a Content-Disposition, JSON files included), pass through untouched, and
// a JSON body that needs no change is sent byte for byte.
func Middleware(isAdmin func(*http.Request) bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !strings.HasPrefix(r.URL.Path, "/api/") || isAdmin(r) {
				next.ServeHTTP(w, r)
				return
			}
			rw := &redactWriter{ResponseWriter: w}
			next.ServeHTTP(rw, r)
			rw.finish()
		})
	}
}

type redactWriter struct {
	http.ResponseWriter
	status      int
	decided     bool
	buffering   bool
	wroteHeader bool
	buf         bytes.Buffer
}

func (w *redactWriter) decide() {
	if w.decided {
		return
	}
	w.decided = true
	ct := w.Header().Get("Content-Type")
	w.buffering = strings.HasPrefix(strings.ToLower(ct), "application/json") && w.Header().Get("Content-Disposition") == ""
}

func (w *redactWriter) WriteHeader(code int) {
	if w.decided && w.wroteHeader {
		return
	}
	w.decide()
	w.status = code
	w.wroteHeader = true
	if !w.buffering {
		w.ResponseWriter.WriteHeader(code)
	}
}

func (w *redactWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if w.buffering {
		return w.buf.Write(b)
	}
	return w.ResponseWriter.Write(b)
}

func (w *redactWriter) Flush() {
	if !w.buffering {
		if f, ok := w.ResponseWriter.(http.Flusher); ok {
			f.Flush()
		}
	}
}

func (w *redactWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("publicerr: underlying writer does not support hijacking")
	}
	return h.Hijack()
}

func (w *redactWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *redactWriter) finish() {
	if !w.buffering {
		return
	}
	body := w.buf.Bytes()
	out := body
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var parsed any
	if err := dec.Decode(&parsed); err == nil {
		redacted := RedactBody(parsed, w.status)
		if !reflect.DeepEqual(parsed, redacted) {
			var b bytes.Buffer
			enc := json.NewEncoder(&b)
			enc.SetEscapeHTML(false)
			if enc.Encode(redacted) == nil {
				out = bytes.TrimRight(b.Bytes(), "\n")
			}
		}
	}
	h := w.Header()
	h.Set("Content-Length", strconv.Itoa(len(out)))
	w.ResponseWriter.WriteHeader(w.status)
	_, _ = w.ResponseWriter.Write(out)
}
