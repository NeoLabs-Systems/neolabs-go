package account

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"strings"
	"time"
)

// Format is the shape of an export download.
type Format int

// Export formats.
const (
	JSON Format = iota // one JSON document
	Zip                // manifest.json, data.json and the account's files
)

// Field is one top-level key of an export, in order.
type Field struct {
	Key   string
	Value any
}

// Section is one block of the account's data. Value returns anything
// encoding/json can write, or a Streamer for data too large to hold.
type Section struct {
	Key   string
	Value func(ctx context.Context, a Account) (any, error)
}

// Streamer writes its own JSON value.
type Streamer interface {
	StreamJSON(ctx context.Context, w io.Writer) error
}

// File is one stored file of the account, added to a Zip export.
type File struct {
	Name     string // slash-separated path inside the archive
	Modified time.Time
	Open     func() (io.ReadCloser, error)
}

// Exporter builds an account's data export from sections the app supplies.
// The export carries only what the sections return: keep password and token
// hashes, secrets and other accounts' data out of them (see Redactor).
type Exporter struct {
	Format   Format
	Filename func(a Account, now time.Time) string
	// Head returns the leading fields: the top of the JSON document, or
	// manifest.json in a Zip.
	Head func(ctx context.Context, a Account, now time.Time) ([]Field, error)
	// Nest puts the sections under one key ("data") instead of beside the
	// head fields.
	Nest     string
	Sections []Section
	// Files adds the account's stored files to a Zip export.
	Files func(ctx context.Context, a Account, add func(File) error) error
	Now   func() time.Time
}

func (e *Exporter) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

// ContentType is the download's media type.
func (e *Exporter) ContentType() string {
	if e.Format == Zip {
		return "application/zip"
	}
	return "application/json; charset=utf-8"
}

// WriteTo writes the account's export.
func (e *Exporter) WriteTo(ctx context.Context, w io.Writer, a Account, now time.Time) error {
	var head []Field
	if e.Head != nil {
		var err error
		if head, err = e.Head(ctx, a, now); err != nil {
			return err
		}
	}
	if e.Format != Zip {
		return e.writeDocument(ctx, w, a, head)
	}
	z := zip.NewWriter(w)
	part := func(name string, modified time.Time) (io.Writer, error) {
		return z.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate, Modified: modified})
	}
	manifest, err := part("manifest.json", now)
	if err != nil {
		return err
	}
	if err := writeObject(ctx, manifest, head, nil); err != nil {
		return err
	}
	data, err := part("data.json", now)
	if err != nil {
		return err
	}
	if err := e.writeDocument(ctx, data, a, nil); err != nil {
		return err
	}
	if e.Files != nil {
		err := e.Files(ctx, a, func(f File) error {
			name, err := archiveName(f.Name)
			if err != nil {
				return err
			}
			r, err := f.Open()
			if err != nil {
				return err
			}
			defer r.Close()
			dst, err := part(name, f.Modified)
			if err != nil {
				return err
			}
			_, err = io.Copy(dst, r)
			return err
		})
		if err != nil {
			return err
		}
	}
	return z.Close()
}

// archiveName refuses names that would escape the archive or replace its
// own parts.
func archiveName(name string) (string, error) {
	clean := path.Clean(strings.ReplaceAll(name, "\\", "/"))
	if name == "" || path.IsAbs(clean) || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") ||
		clean == "manifest.json" || clean == "data.json" {
		return "", fmt.Errorf("account: export file name %q is not allowed", name)
	}
	return clean, nil
}

func (e *Exporter) writeDocument(ctx context.Context, w io.Writer, a Account, head []Field) error {
	sections := func(w io.Writer, first bool) error { return e.writeSections(ctx, w, a, first) }
	if e.Nest == "" {
		return writeObject(ctx, w, head, sections)
	}
	nested := append(head[:len(head):len(head)], Field{Key: e.Nest, Value: streamFunc(func(ctx context.Context, w io.Writer) error {
		if _, err := io.WriteString(w, "{"); err != nil {
			return err
		}
		if err := sections(w, true); err != nil {
			return err
		}
		_, err := io.WriteString(w, "}")
		return err
	})})
	return writeObject(ctx, w, nested, nil)
}

func (e *Exporter) writeSections(ctx context.Context, w io.Writer, a Account, first bool) error {
	for _, s := range e.Sections {
		if err := ctx.Err(); err != nil {
			return err
		}
		v, err := s.Value(ctx, a)
		if err != nil {
			return fmt.Errorf("account: export section %s: %w", s.Key, err)
		}
		if err := writeMember(ctx, w, s.Key, v, first); err != nil {
			return err
		}
		first = false
	}
	return nil
}

type streamFunc func(ctx context.Context, w io.Writer) error

func (f streamFunc) StreamJSON(ctx context.Context, w io.Writer) error { return f(ctx, w) }

// writeObject writes {fields..., more...}; more appends members after the
// fields and is told whether it writes the first one.
func writeObject(ctx context.Context, w io.Writer, fields []Field, more func(w io.Writer, first bool) error) error {
	if _, err := io.WriteString(w, "{"); err != nil {
		return err
	}
	for i, f := range fields {
		if err := writeMember(ctx, w, f.Key, f.Value, i == 0); err != nil {
			return err
		}
	}
	if more != nil {
		if err := more(w, len(fields) == 0); err != nil {
			return err
		}
	}
	_, err := io.WriteString(w, "}")
	return err
}

func writeMember(ctx context.Context, w io.Writer, key string, v any, first bool) error {
	if !first {
		if _, err := io.WriteString(w, ","); err != nil {
			return err
		}
	}
	if err := writeJSON(w, key); err != nil {
		return err
	}
	if _, err := io.WriteString(w, ":"); err != nil {
		return err
	}
	if s, ok := v.(Streamer); ok {
		return s.StreamJSON(ctx, w)
	}
	return writeJSON(w, v)
}

// writeJSON encodes one value without HTML escaping or a trailing newline.
func writeJSON(w io.Writer, v any) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return err
	}
	_, err := w.Write(bytes.TrimSuffix(buf.Bytes(), []byte("\n")))
	return err
}

// Rows streams a query's result as a JSON array of objects whose keys are
// the column names in query order.
type Rows struct {
	DB     RowsQuerier
	Query  string
	Args   []any
	Redact *Redactor // nil keeps every value; select only exportable columns
	// Blob encodes BLOB values; base64 text when nil.
	Blob func([]byte) any
}

// StreamJSON implements Streamer.
func (q Rows) StreamJSON(ctx context.Context, w io.Writer) error {
	rows, err := q.DB.QueryContext(ctx, q.Query, q.Args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return err
	}
	keys := make([][]byte, len(cols))
	for i, c := range cols {
		var buf bytes.Buffer
		if err := writeJSON(&buf, c); err != nil {
			return err
		}
		keys[i] = buf.Bytes()
	}
	ew := &errWriter{w: w}
	ew.str("[")
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	for n := 0; rows.Next() && ew.err == nil; n++ {
		if err := rows.Scan(ptrs...); err != nil {
			return err
		}
		if n > 0 {
			ew.str(",")
		}
		ew.str("{")
		for i, c := range cols {
			if i > 0 {
				ew.str(",")
			}
			ew.write(keys[i])
			ew.str(":")
			v := vals[i]
			if b, ok := v.([]byte); ok && q.Blob != nil {
				v = q.Blob(b)
			}
			if q.Redact != nil {
				v = q.Redact.Value(c, v)
			}
			if ew.err == nil {
				ew.err = writeJSON(w, v)
			}
		}
		ew.str("}")
	}
	if err := rows.Err(); err != nil {
		return err
	}
	ew.str("]")
	return ew.err
}

// errWriter keeps the first write error so a run of small writes needs one
// check.
type errWriter struct {
	w   io.Writer
	err error
}

func (e *errWriter) write(b []byte) {
	if e.err == nil {
		_, e.err = e.w.Write(b)
	}
}

func (e *errWriter) str(s string) { e.write([]byte(s)) }

// SafeFilename keeps a download name to letters, digits, dot, dash and
// underscore.
func SafeFilename(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	if s := strings.Trim(b.String(), ".-"); s != "" {
		return s
	}
	return "export"
}
