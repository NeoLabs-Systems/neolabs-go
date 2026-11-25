package wsgate

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

const (
	writeTimeout = 10 * time.Second
	pongTimeout  = 10 * time.Second
)

// ErrClosed is returned when sending on a connection that has ended.
var ErrClosed = errors.New("wsgate: connection closed")

// Conn is an accepted WebSocket with size limits applied. Reads come from one
// goroutine (Read); sends are safe from any goroutine.
type Conn struct {
	ws     *websocket.Conn
	gate   *Gate
	remote string

	once sync.Once
}

// RemoteAddr is the client address the gate admitted (proxy-aware).
func (c *Conn) RemoteAddr() string { return c.remote }

// Read returns the next message and whether it was binary. The error ends
// the connection; a close from the peer or a message over the size limit are
// both reported here.
func (c *Conn) Read(ctx context.Context) (binary bool, data []byte, err error) {
	typ, data, err := c.ws.Read(ctx)
	if err != nil {
		return false, nil, err
	}
	return typ == websocket.MessageBinary, data, nil
}

// SendText sends a text message.
func (c *Conn) SendText(data []byte) error { return c.write(websocket.MessageText, data) }

// SendBinary sends a binary message.
func (c *Conn) SendBinary(data []byte) error { return c.write(websocket.MessageBinary, data) }

// SendJSON sends v as JSON text, written like JSON.stringify (no HTML escaping).
func (c *Conn) SendJSON(v any) error {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return err
	}
	return c.write(websocket.MessageText, []byte(strings.TrimSuffix(b.String(), "\n")))
}

func (c *Conn) write(typ websocket.MessageType, data []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), writeTimeout)
	defer cancel()
	if err := c.ws.Write(ctx, typ, data); err != nil {
		return ErrClosed
	}
	return nil
}

// Close ends the connection with a close frame; the reason is cut to the
// 123 bytes the protocol allows.
func (c *Conn) Close(code websocket.StatusCode, reason string) {
	c.finish()
	if len(reason) > 123 {
		reason = reason[:123]
	}
	_ = c.ws.Close(code, reason)
}

// Terminate drops the connection without a closing handshake. A gateway calls
// Terminate (or Close) when its Read loop ends, which also releases the
// connection from the gate.
func (c *Conn) Terminate() {
	c.finish()
	_ = c.ws.CloseNow()
}

func (c *Conn) finish() { c.once.Do(func() { c.gate.forget(c) }) }

// KeepAlive pings the client every interval and terminates the connection if
// a ping is not answered in time, until ctx ends or the connection does. Run
// it in its own goroutine next to the Read loop.
func (c *Conn) KeepAlive(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		pctx, cancel := context.WithTimeout(ctx, pongTimeout)
		err := c.ws.Ping(pctx)
		cancel()
		if err != nil {
			if ctx.Err() == nil {
				c.Terminate()
			}
			return
		}
	}
}

func sameHost(origin, host string) bool {
	u, err := url.Parse(origin)
	return err == nil && strings.EqualFold(u.Host, host)
}
