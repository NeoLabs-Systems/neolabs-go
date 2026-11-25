package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type echoIn struct {
	Text string `json:"text"`
}
type echoOut struct {
	Echo string `json:"echo"`
}

func newServer() *sdk.Server {
	s := sdk.NewServer(&sdk.Implementation{Name: "test", Version: "1"}, nil)
	sdk.AddTool(s, &sdk.Tool{Name: "echo", Description: "echoes"}, func(_ context.Context, _ *sdk.CallToolRequest, in echoIn) (*sdk.CallToolResult, echoOut, error) {
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: in.Text}}}, echoOut{Echo: in.Text}, nil
	})
	return s
}

func local() *http.Client { return &http.Client{} }

func TestTransports(t *testing.T) {
	cases := map[string]struct {
		handler http.Handler
		tr      Transport
	}{
		"streamable": {sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return newServer() }, nil), StreamableHTTP},
		"sse":        {sdk.NewSSEHandler(func(*http.Request) *sdk.Server { return newServer() }, nil), SSE},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			var gotKey string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if k := r.Header.Get("X-Api-Key"); k != "" {
					gotKey = k
				}
				c.handler.ServeHTTP(w, r)
			}))
			defer srv.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			s, err := Connect(ctx, Config{Endpoint: srv.URL, Transport: c.tr, HTTPClient: local(), Headers: map[string]string{"X-Api-Key": "k1"}})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			tools, err := s.Tools(ctx)
			if err != nil || len(tools) != 1 || tools[0]["name"] != "echo" {
				t.Fatalf("tools %v %v", tools, err)
			}
			if _, ok := tools[0]["inputSchema"]; !ok {
				t.Fatal("no inputSchema")
			}
			res, err := s.Call(ctx, "echo", map[string]any{"text": "hi"})
			if err != nil {
				t.Fatal(err)
			}
			content := res["content"].([]any)[0].(map[string]any)
			if content["text"] != "hi" {
				t.Fatalf("result %v", res)
			}
			if gotKey != "k1" {
				t.Fatalf("header not sent: %q", gotKey)
			}
		})
	}
}

func TestDefaultClientRefusesLoopback(t *testing.T) {
	srv := httptest.NewServer(sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return newServer() }, nil))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := Connect(ctx, Config{Endpoint: srv.URL, Transport: StreamableHTTP}); err == nil {
		t.Fatal("loopback reached through the default client")
	}
}

func TestUnknownTransport(t *testing.T) {
	if _, err := Connect(context.Background(), Config{Endpoint: "http://x", Transport: "ws"}); err == nil {
		t.Fatal("expected error")
	}
}
