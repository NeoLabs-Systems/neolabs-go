package ollama

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

func TestEmbeddingUsesExplicitCapabilities(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			_, _ = w.Write([]byte(`{"models":[{"name":"declared","capabilities":["embedding"]},{"name":"embedding-name-only"}]}`))
		case "/api/embed":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode request: %v", err)
			}
			if body["model"] != "declared" {
				t.Errorf("model=%v", body["model"])
			}
			_, _ = w.Write([]byte(`{"model":"declared:latest","embeddings":[[0.5,0.5],[1,0]],"prompt_eval_count":7}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	p := New(Config{Config: transport.Config{BaseURL: server.URL, TrustedBaseURL: true, HTTPClient: server.Client()}})
	catalog, err := p.EmbeddingModels(context.Background())
	if err != nil || len(catalog) != 1 || catalog[0].ID != "declared" || catalog[0].Kind != llm.KindEmbedding {
		t.Fatalf("catalog=%#v err=%v", catalog, err)
	}
	result, err := p.Embed(context.Background(), []string{"a", "b"}, llm.Options{Model: "declared"})
	if err != nil || !reflect.DeepEqual(result.Vectors, [][]float32{{0.5, 0.5}, {1, 0}}) || result.Model != "declared:latest" || result.Usage.InputTokens != 7 {
		t.Fatalf("result=%#v err=%v", result, err)
	}
}
