// Package wsclient is the WebSocket layer of the NeoLabs apps: a client with
// bounded reads, timeouts and reconnect with backoff, plus the server-side
// accept, so apps never depend on a WebSocket library directly.
package wsclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/coder/websocket"
)

// DefaultReadLimit bounds one message unless Options say otherwise.
const DefaultReadLimit = 16 << 20

// Close codes used by the apps.
const (
	CloseNormal          = int(websocket.StatusNormalClosure)
	ClosePolicyViolation = int(websocket.StatusPolicyViolation)
)

// Options tune Dial.
type Options struct {
	Header           http.Header
	HTTPClient       *http.Client // the SSRF-safe client when the address is user influenced
	ReadLimit        int64        // bytes per message, DefaultReadLimit when zero
	HandshakeTimeout time.Duration
}

// Conn is one WebSocket connection, client or server side.
type Conn struct{ c *websocket.Conn }

// Dial opens a client connection. url may use http(s) or ws(s).
func Dial(ctx context.Context, url string, o Options) (*Conn, error) {
	if o.HandshakeTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, o.HandshakeTimeout)
		defer cancel()
	}
	c, resp, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: o.Header, HTTPClient: o.HTTPClient})
	if err != nil {
		if resp != nil {
			return nil, fmt.Errorf("websocket handshake failed (%d): %w", resp.StatusCode, err)
		}
		return nil, err
	}
	c.SetReadLimit(limitOrDefault(o.ReadLimit))
	return &Conn{c: c}, nil
}

// AcceptOptions tune Accept.
type AcceptOptions struct {
	ReadLimit int64
	// OriginPatterns lists extra origins (host patterns) allowed besides the
	// request's own host. A request without an Origin header (a device, not a
	// browser) is always accepted.
	OriginPatterns []string
}

// Accept upgrades a request to a WebSocket. On failure it has already
// answered the request.
func Accept(w http.ResponseWriter, r *http.Request, o AcceptOptions) (*Conn, error) {
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: o.OriginPatterns})
	if err != nil {
		return nil, err
	}
	c.SetReadLimit(limitOrDefault(o.ReadLimit))
	return &Conn{c: c}, nil
}

func limitOrDefault(n int64) int64 {
	if n <= 0 {
		return DefaultReadLimit
	}
	return n
}

// Read waits for the next text or binary message.
func (c *Conn) Read(ctx context.Context) (binary bool, data []byte, err error) {
	typ, data, err := c.c.Read(ctx)
	return typ == websocket.MessageBinary, data, err
}

// WriteText sends one text message.
func (c *Conn) WriteText(ctx context.Context, data []byte) error {
	return c.c.Write(ctx, websocket.MessageText, data)
}

// WriteJSON sends v as one text message, encoded without HTML escaping like
// JSON.stringify does.
func (c *Conn) WriteJSON(ctx context.Context, v any) error {
	data, err := Marshal(v)
	if err != nil {
		return err
	}
	return c.WriteText(ctx, data)
}

// Ping waits for the peer's pong.
func (c *Conn) Ping(ctx context.Context) error { return c.c.Ping(ctx) }

// Close ends the connection with a close handshake.
func (c *Conn) Close(code int, reason string) error {
	return c.c.Close(websocket.StatusCode(code), truncate(reason, 120))
}

// CloseNow drops the connection without waiting for the peer.
func (c *Conn) CloseNow() error { return c.c.CloseNow() }

// CloseStatus is the close code carried by err, or -1.
func CloseStatus(err error) int {
	var ce websocket.CloseError
	if errors.As(err, &ce) {
		return int(ce.Code)
	}
	return -1
}

// CloseReason is the close reason carried by err, or "".
func CloseReason(err error) string {
	var ce websocket.CloseError
	if errors.As(err, &ce) {
		return ce.Reason
	}
	return ""
}

// Marshal encodes like JSON.stringify: no HTML escaping, no trailing newline.
func Marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// truncate keeps a close reason inside the 123 bytes the protocol allows.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
