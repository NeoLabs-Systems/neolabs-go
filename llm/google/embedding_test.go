package google

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/NeoLabs-Systems/neolabs-go/llm"
	"github.com/NeoLabs-Systems/neolabs-go/llm/transport"
)

func TestEmbeddingCatalogIncludesEmbedOnlyModels(t *testing.T) {
	var requested int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1beta/models" {
			requested++
			if r.Header.Get("x-goog-api-key") != "google-key" {
				t.Errorf("missing Google API key")
			}
			_, _ = w.Write([]byte(`{"models":[{"name":"models/vector-model","displayName":"Vector","supportedGenerationMethods":["embedContent"]},{"name":"models/chat-model","supportedGenerationMethods":["generateContent"]}]}`))
			return
		}
		if r.URL.Path != "/v1beta/models/vector-model:batchEmbedContents" {
			http.NotFound(w, r)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		requests, _ := body["requests"].([]any)
		if len(requests) != 2 {
			t.Errorf("request count=%d", len(requests))
		}
		_, _ = w.Write([]byte(`{"embeddings":[{"values":[1,0]},{"values":[0,1]}]}`))
	}))
	defer server.Close()
	p := New(transport.Config{APIKey: "google-key", BaseURL: server.URL, TrustedBaseURL: true, HTTPClient: server.Client()}, nil)
	catalog, err := p.EmbeddingModels(context.Background())
	if err != nil || len(catalog) != 1 || catalog[0].ID != "vector-model" || catalog[0].Kind != llm.KindEmbedding {
		t.Fatalf("catalog=%#v err=%v", catalog, err)
	}
	result, err := p.Embed(context.Background(), []string{"a", "b"}, llm.Options{Model: "vector-model"})
	if err != nil || !reflect.DeepEqual(result.Vectors, [][]float32{{1, 0}, {0, 1}}) || result.Model != "vector-model" {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if requested != 1 {
		t.Fatalf("catalog requests=%d", requested)
	}
}
