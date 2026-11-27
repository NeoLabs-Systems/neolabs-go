package publicerr

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"testing"
)

type vectors struct {
	Home      string
	Root      string
	Generic   string
	Technical []struct {
		Text      string
		Technical bool
	}
	Redact []struct {
		Body   any
		Status int
		Result any
	}
	Sanitize []struct{ In, Out string }
	Codes    []struct {
		Err      any
		Public   *string
		NonAdmin string
	}
}

func load(t *testing.T) vectors {
	t.Helper()
	raw, err := os.ReadFile("testdata/vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var v vectors
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestGenericMatches(t *testing.T) {
	if load(t).Generic != Generic {
		t.Fatal("generic message drifted from the Node servers")
	}
}

func TestLooksTechnicalMatchesNode(t *testing.T) {
	for _, c := range load(t).Technical {
		if got := LooksTechnical(c.Text); got != c.Technical {
			t.Errorf("%.60q: got %v want %v", c.Text, got, c.Technical)
		}
	}
}

func TestRedactBodyMatchesNode(t *testing.T) {
	for i, c := range load(t).Redact {
		got := RedactBody(c.Body, c.Status)
		if !reflect.DeepEqual(got, c.Result) {
			t.Errorf("case %d (status %d)\n got: %#v\nwant: %#v", i, c.Status, got, c.Result)
		}
	}
}

func TestSanitizeMatchesNode(t *testing.T) {
	v := load(t)
	for _, c := range v.Sanitize {
		if got := Sanitize(c.In, v.Home, v.Root); got != c.Out {
			t.Errorf("%q\n got: %q\nwant: %q", c.In, got, c.Out)
		}
	}
}

func TestNonAdminMessageAndCodesMatchNode(t *testing.T) {
	v := load(t)
	for i, c := range v.Codes {
		f := Failure{}
		switch e := c.Err.(type) {
		case string:
			f.IsString, f.Message = true, e
		case map[string]any:
			if s, ok := e["message"].(string); ok {
				f.Message = s
			}
			if s, ok := e["code"].(string); ok {
				f.Code = s
			}
			for _, k := range []string{"status", "statusCode"} {
				if n, ok := e[k].(json.Number); ok {
					x, _ := n.Int64()
					f.Status = int(x)
				}
			}
		}
		wantCode := ""
		if c.Public != nil {
			wantCode = *c.Public
		}
		if got := PublicCode(f.Code); got != wantCode {
			t.Errorf("case %d: public code %q want %q", i, got, wantCode)
		}
		if got := NonAdminMessage(f, v.Home, v.Root); got != c.NonAdmin {
			t.Errorf("case %d: non-admin %q want %q", i, got, c.NonAdmin)
		}
	}
}

func apiHandler(status int, contentType, body string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		w.WriteHeader(status)
		w.Write([]byte(body))
	})
}

func serve(h http.Handler, path string, admin bool) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	Middleware(func(*http.Request) bool { return admin })(h).ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
	return rec
}

func TestMiddleware(t *testing.T) {
	tech := `{"error":"TypeError: x is not a function","code":"BAD_THING","n":12345678901234567890}`
	rec := serve(apiHandler(400, "application/json; charset=utf-8", tech), "/api/x", false)
	var got map[string]any
	json.Unmarshal(rec.Body.Bytes(), &got)
	if rec.Code != 400 || got["error"] != Generic || got["code"] != "BAD_THING" {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Content-Length") == "" {
		t.Fatal("content length must be recomputed")
	}

	// 5xx hides the cause entirely.
	rec = serve(apiHandler(500, "application/json", `{"error":"db exploded","stack":"s"}`), "/api/x", false)
	if rec.Body.String() != `{"error":"`+Generic+`"}` {
		t.Fatalf("%s", rec.Body.String())
	}

	// Untouched bodies are byte for byte, including key order and big numbers.
	clean := `{"z":1,"a":{"n":12345678901234567890},"error":"Invalid credentials"}`
	if rec = serve(apiHandler(401, "application/json", clean), "/api/x", false); rec.Body.String() != clean {
		t.Fatalf("clean body changed: %s", rec.Body.String())
	}

	// Admins, non-API paths and non-JSON responses are untouched.
	if rec = serve(apiHandler(500, "application/json", tech), "/api/x", true); rec.Body.String() != tech {
		t.Fatal("admin must see the raw error")
	}
	if rec = serve(apiHandler(500, "application/json", tech), "/other", false); rec.Body.String() != tech {
		t.Fatal("non-api path must not be touched")
	}
	if rec = serve(apiHandler(500, "text/event-stream", "data: TypeError: boom\n\n"), "/api/stream", false); rec.Body.String() != "data: TypeError: boom\n\n" {
		t.Fatal("streams must pass through")
	}
}

func TestMiddlewareLeavesDownloadsAlone(t *testing.T) {
	file := `{"error":"TypeError: boom at /var/log/app"}`
	rec := serve(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Disposition", `inline; filename="trace.json"`)
		w.Write([]byte(file))
	}), "/api/files/x/content", false)
	if rec.Body.String() != file {
		t.Fatalf("download changed: %s", rec.Body.String())
	}
}
