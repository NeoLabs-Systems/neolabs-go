package embedding

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestProviderRequestsAndDimensions(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		switch r.URL.Path {
		case "/openai":
			if r.Header.Get("Authorization") != "Bearer secret" || body["model"] != OpenAIModel {
				t.Error("invalid OpenAI request")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"embedding": make([]float64, OpenAIDimensions)}}})
		case "/google":
			if r.Header.Get("x-goog-api-key") != "secret" {
				t.Error("invalid Google key")
			}
			content := body["content"].(map[string]any)
			part := content["parts"].([]any)[0].(map[string]any)
			if !strings.HasPrefix(part["text"].(string), "task: search result | query: ") {
				t.Error("invalid query prefix")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"embedding": map[string]any{"values": make([]float64, GoogleDimensions)}})
		}
	}))
	defer server.Close()
	client := &Client{HTTP: server.Client(), OpenAIURL: server.URL + "/openai", GoogleURL: server.URL + "/google"}
	for _, tc := range []struct {
		name       string
		run        func() (*Result, error)
		dimensions int
	}{
		{"openai", func() (*Result, error) { return client.OpenAI(context.Background(), "secret", "text") }, OpenAIDimensions},
		{"google", func() (*Result, error) { return client.Google(context.Background(), "secret", "text", "query") }, GoogleDimensions},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := tc.run()
			if err != nil || result == nil || len(result.Vector) != tc.dimensions {
				t.Fatalf("result = %v, error = %v", result, err)
			}
		})
	}
}
