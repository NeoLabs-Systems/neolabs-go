package httpx

import (
	"compress/gzip"
	"net/http"
	"strings"
)

const compressMinBytes = 1024

// CompressJSON gzips JSON responses of at least 1 KiB for clients that accept
// it. Every other body keeps its exact length and byte ranges.
func CompressJSON() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodHead || r.Header.Get("Upgrade") != "" ||
				!strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
				next.ServeHTTP(w, r)
				return
			}
			cw := &compressWriter{ResponseWriter: w}
			defer cw.close()
			next.ServeHTTP(cw, r)
		})
	}
}

// compressWriter holds the status back until the first write shows whether the
// body is worth compressing.
type compressWriter struct {
	http.ResponseWriter
	status  int
	started bool
	gzip    *gzip.Writer
}

func (w *compressWriter) WriteHeader(status int) {
	if status < http.StatusOK {
		w.ResponseWriter.WriteHeader(status)
		return
	}
	if w.status == 0 {
		w.status = status
	}
}

func (w *compressWriter) start(first int) {
	if w.started {
		return
	}
	w.started = true
	if w.status == 0 {
		w.status = http.StatusOK
	}
	header := w.Header()
	contentType, _, _ := strings.Cut(header.Get("Content-Type"), ";")
	if strings.EqualFold(strings.TrimSpace(contentType), "application/json") {
		header.Add("Vary", "Accept-Encoding")
		if first >= compressMinBytes && header.Get("Content-Encoding") == "" && header.Get("Content-Range") == "" {
			header.Set("Content-Encoding", "gzip")
			header.Del("Content-Length")
			w.gzip = gzip.NewWriter(w.ResponseWriter)
		}
	}
	w.ResponseWriter.WriteHeader(w.status)
}

func (w *compressWriter) Write(data []byte) (int, error) {
	w.start(len(data))
	if w.gzip != nil {
		return w.gzip.Write(data)
	}
	return w.ResponseWriter.Write(data)
}

// FlushError sends what is buffered, for streaming handlers.
func (w *compressWriter) FlushError() error {
	w.start(0)
	if w.gzip != nil {
		if err := w.gzip.Flush(); err != nil {
			return err
		}
	}
	return http.NewResponseController(w.ResponseWriter).Flush()
}

func (w *compressWriter) Flush() { _ = w.FlushError() }

// Unwrap lets http.ResponseController reach deadlines and hijacking.
func (w *compressWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *compressWriter) close() {
	if w.status != 0 {
		w.start(0)
	}
	if w.gzip != nil {
		_ = w.gzip.Close()
	}
}
