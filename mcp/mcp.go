// Package mcp is a small MCP client for remote servers: connect over
// streamable HTTP or legacy SSE, list tools, call tools, close. It wraps the
// official go-sdk so apps share one way to reach MCP servers, with outbound
// requests going through the ssrf-checked client by default.
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/NeoLabs-Systems/neolabs-go/ssrf"
	"github.com/modelcontextprotocol/go-sdk/auth"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Transport names the wire protocol of a remote server.
type Transport string

const (
	// SSE is the legacy HTTP+SSE transport.
	SSE Transport = "sse"
	// StreamableHTTP is the current streamable HTTP transport.
	StreamableHTTP Transport = "streamable-http"
)

// Config describes one connection.
type Config struct {
	Endpoint  string
	Transport Transport // empty means SSE
	// Headers are sent with every request (API keys, bearer tokens).
	Headers map[string]string
	// HTTPClient carries the requests. Nil uses an ssrf client that refuses
	// non-public destinations; it has no overall timeout because MCP streams
	// stay open, so bound operations with the contexts you pass in.
	HTTPClient *http.Client
	// OAuth authorizes requests on the streamable transport. On the SSE
	// transport only its current token is attached; the SDK cannot run the
	// authorization flow there.
	OAuth auth.OAuthHandler
	// Name and Version identify this client to the server.
	Name    string
	Version string
}

// Session is a live connection.
type Session struct{ cs *sdk.ClientSession }

// DefaultHTTPClient is the ssrf client used when Config.HTTPClient is nil.
func DefaultHTTPClient() *http.Client {
	return ssrf.NewClient(ssrf.Options{Timeout: -1})
}

type headerTransport struct {
	base    http.RoundTripper
	headers map[string]string
	oauth   auth.OAuthHandler
}

func (t headerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	for k, v := range t.headers {
		r.Header.Set(k, v)
	}
	if t.oauth != nil {
		if src, err := t.oauth.TokenSource(r.Context()); err == nil && src != nil {
			if tok, err := src.Token(); err == nil {
				tok.SetAuthHeader(r)
			}
		}
	}
	return t.base.RoundTrip(r)
}

func (c Config) transport() (sdk.Transport, error) {
	client := c.HTTPClient
	if client == nil {
		client = DefaultHTTPClient()
	}
	switch c.Transport {
	case StreamableHTTP:
		return &sdk.StreamableClientTransport{Endpoint: c.Endpoint, HTTPClient: withHeaders(client, c.Headers, nil), OAuthHandler: c.OAuth}, nil
	case SSE, "":
		return &sdk.SSEClientTransport{Endpoint: c.Endpoint, HTTPClient: withHeaders(client, c.Headers, c.OAuth)}, nil
	}
	return nil, fmt.Errorf("mcp: unknown transport %q", c.Transport)
}

func withHeaders(client *http.Client, headers map[string]string, oauth auth.OAuthHandler) *http.Client {
	if len(headers) == 0 && oauth == nil {
		return client
	}
	base := client.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	clone := *client
	clone.Transport = headerTransport{base: base, headers: headers, oauth: oauth}
	return &clone
}

// Connect opens a session and completes the MCP initialize handshake.
func Connect(ctx context.Context, c Config) (*Session, error) {
	t, err := c.transport()
	if err != nil {
		return nil, err
	}
	name, version := c.Name, c.Version
	if name == "" {
		name = "neolabs"
	}
	if version == "" {
		version = "1.0.0"
	}
	cs, err := sdk.NewClient(&sdk.Implementation{Name: name, Version: version}, nil).Connect(ctx, t, nil)
	if err != nil {
		return nil, err
	}
	return &Session{cs: cs}, nil
}

// Tools lists every tool the server offers, following pagination. Each tool is
// the JSON object the server described (name, description, inputSchema, ...).
func (s *Session) Tools(ctx context.Context) ([]map[string]any, error) {
	out := []map[string]any{}
	for tool, err := range s.cs.Tools(ctx, nil) {
		if err != nil {
			return nil, err
		}
		raw, err := json.Marshal(tool)
		if err != nil {
			return nil, err
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}

// Call runs a tool and returns the server's result object (content, isError,
// structuredContent) as decoded JSON.
func (s *Session) Call(ctx context.Context, name string, args map[string]any) (map[string]any, error) {
	res, err := s.cs.CallTool(ctx, &sdk.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(res)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// Close ends the session.
func (s *Session) Close() error {
	if s == nil || s.cs == nil {
		return errors.New("mcp: session not open")
	}
	return s.cs.Close()
}

// Wait blocks until the connection ends and returns why.
func (s *Session) Wait() error { return s.cs.Wait() }
