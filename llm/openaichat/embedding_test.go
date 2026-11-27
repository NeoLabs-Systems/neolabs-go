package openaichat

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

func TestEmbeddingProviderAndExplicitCatalogKind(t *testing.T) {
	var received map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"declared-vector","kind":"embedding"},{"id":"vector-name-only"}]}`))
		case "/v1/embeddings":
			if r.Header.Get("Authorization") != "Bearer fixture-key" {
				t.Errorf("authorization=%q", r.Header.Get("Authorization"))
			}
			if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
				t.Errorf("decode body: %v", err)
			}
			_, _ = w.Write([]byte(`{"model":"fixture-embedding","data":[{"index":1,"embedding":[0,1]},{"index":0,"embedding":[1,0]}],"usage":{"prompt_tokens":5}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	p := NewCompatible(transport.Config{APIKey: "fixture-key", BaseURL: server.URL + "/v1", TrustedBaseURL: true, HTTPClient: server.Client()})
	catalog, err := p.EmbeddingModels(context.Background())
	if err != nil || len(catalog) != 1 || catalog[0].ID != "declared-vector" || catalog[0].Kind != llm.KindEmbedding {
		t.Fatalf("embedding catalog=%#v err=%v", catalog, err)
	}
	result, err := p.Embed(context.Background(), []string{"first", "second"}, llm.Options{Model: "declared-vector"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(result.Vectors, [][]float32{{1, 0}, {0, 1}}) || result.Model != "fixture-embedding" || result.Usage == nil || result.Usage.InputTokens != 5 {
		t.Fatalf("embedding result=%#v", result)
	}
	if received["model"] != "declared-vector" || received["encoding_format"] != "float" {
		t.Fatalf("embedding request=%v", received)
	}
	if _, err := p.Embed(context.Background(), []string{"first"}, llm.Options{}); llm.AsError(err).Code != llm.CodeModelSelectionRequired {
		t.Fatalf("missing model error=%v", err)
	}
}

func TestOpenAIEmbeddingCatalogUsesRawExplicitMetadata(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"text-embedding-3-small","kind":"embedding"},{"id":"embedding-name-only"},{"id":"text-embedding-vendor","capabilities":["embedding"]}]}`))
	}))
	defer server.Close()
	p := New(transport.Config{APIKey: "fixture", BaseURL: server.URL + "/v1", TrustedBaseURL: true, HTTPClient: server.Client()})
	models, err := p.EmbeddingModels(context.Background())
	if err != nil || len(models) != 2 || models[0].ID != "text-embedding-3-small" || models[1].ID != "text-embedding-vendor" {
		t.Fatalf("models=%#v err=%v", models, err)
	}
}

func TestOpenRouterEmbeddingCatalogUsesExplicitModality(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/models" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"vendor/embed","architecture":{"modality":"text->embedding"}},{"id":"vendor/embedding-chat","architecture":{"modality":"text->text"}}]}`))
	}))
	defer server.Close()
	p := NewOpenRouter(transport.Config{APIKey: "fixture", BaseURL: server.URL + "/api/v1", TrustedBaseURL: true, HTTPClient: server.Client()}, nil)
	models, err := p.EmbeddingModels(context.Background())
	if err != nil || len(models) != 1 || models[0].ID != "vendor/embed" || models[0].Kind != llm.KindEmbedding {
		t.Fatalf("models=%#v err=%v", models, err)
	}
}

func TestNeoRouterEmbeddingRoleCatalogAndEndpoint(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			if r.URL.Query().Get("type") != "embedding" {
				t.Errorf("catalog type=%q", r.URL.Query().Get("type"))
			}
			_, _ = w.Write([]byte(`{"data":[{"id":"auto:default"}]}`))
		case "/v1/embeddings":
			_, _ = w.Write([]byte(`{"model":"router-model","data":[{"index":0,"embedding":[0.25,0.75]}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	p := NewNeoRouter(transport.Config{APIKey: "router-key", BaseURL: server.URL, TrustedBaseURL: true, HTTPClient: server.Client()}, nil)
	catalog, err := p.EmbeddingModels(context.Background())
	if err != nil || len(catalog) != 1 || catalog[0].Kind != llm.KindEmbedding {
		t.Fatalf("catalog=%#v err=%v", catalog, err)
	}
	result, err := p.Embed(context.Background(), []string{"sample"}, llm.Options{Model: "auto:default"})
	if err != nil || len(result.Vectors) != 1 || result.Model != "router-model" {
		t.Fatalf("result=%#v err=%v", result, err)
	}
}
