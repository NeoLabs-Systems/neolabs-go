package providers_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/NeoLabs-Systems/neolabs-go/llm"
	"github.com/NeoLabs-Systems/neolabs-go/llm/openaichat"
	"github.com/NeoLabs-Systems/neolabs-go/llm/providers"
	"github.com/NeoLabs-Systems/neolabs-go/llm/transport"
)

func TestImagePartsAndJSONModeReachEveryWireFormat(t *testing.T) {
	messages := []llm.Message{{Role: "user", Content: []any{
		map[string]any{"type": "text", "text": "what is this"},
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,QUJD"}},
	}}}
	cases := map[string]struct {
		reply string
		want  []string
	}{
		"openai-compatible": {`{"choices":[{"message":{"content":"{}"}}]}`, []string{`"image_url":{"url":"data:image/png;base64,QUJD"}`, `"response_format":{"type":"json_object"}`}},
		"anthropic":         {`{"content":[{"type":"text","text":"{}"}]}`, []string{`"source":{"type":"base64","media_type":"image/png","data":"QUJD"}`}},
		"google":            {`{"candidates":[{"content":{"parts":[{"text":"{}"}]}}]}`, []string{`"inlineData":{"mimeType":"image/png","data":"QUJD"}`, `"responseMimeType":"application/json"`, `"text":"what is this"`}},
		"ollama":            {`{"message":{"content":"{}"},"done":true}`, []string{`"images":["QUJD"]`, `"content":"what is this"`, `"format":"json"`}},
	}
	for id, test := range cases {
		t.Run(id, func(t *testing.T) {
			var sent string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/tags" {
					_, _ = w.Write([]byte(`{"models":[{"name":"m:latest"}]}`))
					return
				}
				body, _ := io.ReadAll(r.Body)
				sent = string(body)
				_, _ = w.Write([]byte(test.reply))
			}))
			defer server.Close()
			provider, err := providers.New(id, providers.Config{Config: transport.Config{APIKey: "k", BaseURL: server.URL, TrustedBaseURL: true}})
			if err != nil {
				t.Fatal(err)
			}
			result, err := provider.Chat(context.Background(), messages, nil, llm.Options{Model: "m", JSON: true})
			if err != nil || result.Content != "{}" {
				t.Fatalf("result=%+v error=%v", result, err)
			}
			for _, want := range test.want {
				if !strings.Contains(sent, want) {
					t.Errorf("request is missing %s: %s", want, sent)
				}
			}
		})
	}
}

func TestModelListKeepsEveryEntryAndTheKeyLimits(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer k" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"Invalid key"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"text-embedding-3-small"}],"neorouter":{"not_allowed_for_key":{"paid":2}}}`))
	}))
	defer server.Close()
	list, err := openaichat.NewNeoRouter(transport.Config{APIKey: "k", BaseURL: server.URL, TrustedBaseURL: true}, nil).ModelList(context.Background(), "embedding")
	if err != nil || len(list.Data) != 1 || list.NotAllowed["paid"] != 2 {
		t.Fatalf("list=%+v error=%v", list, err)
	}
	_, err = openaichat.NewNeoRouter(transport.Config{APIKey: "wrong", BaseURL: server.URL, TrustedBaseURL: true}, nil).ModelList(context.Background(), "chat")
	if e := llm.AsError(err); e == nil || e.Status != http.StatusUnauthorized {
		t.Fatalf("error=%v", err)
	}
}
