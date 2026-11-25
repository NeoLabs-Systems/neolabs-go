package openaiimages

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGenerate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/images/generations" || r.Header.Get("Authorization") != "Bearer k" {
			t.Errorf("%s %s", r.URL.Path, r.Header.Get("Authorization"))
		}
		var body map[string]any
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		if body["model"] != "grok-imagine-image" || body["n"] != 2.0 || body["response_format"] != "b64_json" {
			t.Errorf("body %v", body)
		}
		if body["prompt"] == "fail" {
			w.WriteHeader(429)
			_, _ = io.WriteString(w, `{"error":{"message":"slow down"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"data":[{"b64_json":"QQ=="},{"b64_json":"Qg=="}]}`)
	}))
	defer srv.Close()
	c := Client{BaseURL: srv.URL + "/v1/", APIKey: "k"}
	got, err := c.Generate(context.Background(), "grok-imagine-image", "cat", 2)
	if err != nil || len(got) != 2 || got[0] != "QQ==" {
		t.Fatalf("%v %v", got, err)
	}
	if _, err := c.Generate(context.Background(), "grok-imagine-image", "fail", 2); err == nil || err.Error() != "429 slow down" {
		t.Fatal(err)
	}
	if _, err := (Client{BaseURL: srv.URL}).Generate(context.Background(), "m", "p", 1); err == nil || err.Error() != MissingKey {
		t.Fatal(err)
	}
}
