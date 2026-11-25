// Package realtime is a Socket.IO v4 server (Engine.IO v4 underneath) for Go.
// It speaks the same wire protocol as the Node `socket.io` package, so the
// socket.io-client libraries (JavaScript and Dart) connect to it unchanged:
// polling and websocket transports with the upgrade probe, namespaces, rooms,
// acknowledgements in both directions, binary attachments, heartbeat and
// connect timeout.
//
// Mount the Hub on the "/socket.io/" path; it is an http.Handler. Who is
// connected is decided by the application: a namespace middleware can read the
// handshake request (Socket.Request) with whatever the HTTP middleware chain
// attached to it, such as a session.
package realtime

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

var errHubClosed = errors.New("realtime: hub is closed")

// Options configure a Hub. Zero values take the defaults of the Node server.
type Options struct {
	PingInterval   time.Duration // Engine.IO heartbeat, default 25s
	PingTimeout    time.Duration // time the client has to answer, default 20s
	ConnectTimeout time.Duration // time to join a namespace after connecting, default 10s
	// MaxPayload bounds one incoming message or polling POST body, default 1 MB.
	MaxPayload int64
	// MaxQueuedPackets bounds what may wait for one client; a client that falls
	// further behind is disconnected. Default 2048.
	MaxQueuedPackets int
	// AllowOrigin decides which browser origins may connect; it is called with
	// the empty string when the request has no Origin header. Nil allows
	// requests without an Origin and requests from the server's own host.
	AllowOrigin func(origin string) bool
	Log         *slog.Logger
}

func (o *Options) fill() {
	if o.PingInterval <= 0 {
		o.PingInterval = 25 * time.Second
	}
	if o.PingTimeout <= 0 {
		o.PingTimeout = 20 * time.Second
	}
	if o.ConnectTimeout <= 0 {
		o.ConnectTimeout = 10 * time.Second
	}
	if o.MaxPayload <= 0 {
		o.MaxPayload = 1_000_000
	}
	if o.MaxQueuedPackets <= 0 {
		o.MaxQueuedPackets = 2048
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
}

// Hub is the server. The methods of its default namespace ("/") are available
// on the Hub itself; Of returns others.
type Hub struct {
	opts Options
	root *Namespace

	mu     sync.RWMutex
	conns  map[string]*conn
	nsps   map[string]*Namespace
	closed bool
}

// New creates a Hub.
func New(opts Options) *Hub {
	opts.fill()
	h := &Hub{opts: opts, conns: map[string]*conn{}, nsps: map[string]*Namespace{}}
	h.root = h.Of("/")
	return h
}

// UserRoom is the room that EmitToUser addresses. The application joins each
// authenticated socket to it.
func UserRoom(userID int64) string { return "user:" + strconv.FormatInt(userID, 10) }

// Of returns the namespace with this name, creating it.
func (h *Hub) Of(name string) *Namespace {
	if !strings.HasPrefix(name, "/") {
		name = "/" + name
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if ns, ok := h.nsps[name]; ok {
		return ns
	}
	ns := &Namespace{hub: h, name: name, sockets: map[string]*Socket{}, rooms: map[string]map[string]*Socket{}}
	h.nsps[name] = ns
	return ns
}

func (h *Hub) lookup(name string) *Namespace {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.nsps[name]
}

// Use adds a connection middleware to the default namespace.
func (h *Hub) Use(mw Middleware) { h.root.Use(mw) }

// OnConnection registers a handler for sockets that join the default namespace.
func (h *Hub) OnConnection(fn func(*Socket)) { h.root.OnConnection(fn) }

// To addresses the sockets in the given rooms of the default namespace.
func (h *Hub) To(rooms ...string) Broadcast { return h.root.To(rooms...) }

// EmitToUser sends an event to every socket in the user's room. A nil payload
// sends the event without arguments.
func (h *Hub) EmitToUser(userID int64, event string, payload any) {
	h.EmitToRoom(UserRoom(userID), event, payload)
}

// EmitToRoom sends an event to every socket in a room of the default namespace.
func (h *Hub) EmitToRoom(room, event string, payload any) {
	var args []any
	if payload != nil {
		args = []any{payload}
	}
	if err := h.root.To(room).Emit(event, args...); err != nil {
		h.opts.Log.Warn("realtime emit failed", "event", event, "error", err)
	}
}

// Sockets lists the sockets in a room of the default namespace.
func (h *Hub) Sockets(room string) []*Socket { return h.root.Sockets(room) }

// Close disconnects every client and refuses new ones.
func (h *Hub) Close() {
	h.mu.Lock()
	h.closed = true
	conns := make([]*conn, 0, len(h.conns))
	for _, c := range h.conns {
		conns = append(conns, c)
	}
	h.mu.Unlock()
	for _, c := range conns {
		c.close("server shutting down")
	}
}

func (h *Hub) register(c *conn) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return false
	}
	h.conns[c.id] = c
	return true
}

func (h *Hub) unregister(c *conn) {
	h.mu.Lock()
	delete(h.conns, c.id)
	h.mu.Unlock()
}

func (h *Hub) conn(id string) *conn {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.conns[id]
}

func randomID() (string, error) {
	var b [15]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

func writeEngineError(w http.ResponseWriter, status, code int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`{"code":` + strconv.Itoa(code) + `,"message":` + strconv.Quote(message) + `}`))
}

func (h *Hub) originAllowed(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if h.opts.AllowOrigin != nil {
		return h.opts.AllowOrigin(origin)
	}
	if origin == "" {
		return true
	}
	i := strings.Index(origin, "://")
	return i >= 0 && strings.EqualFold(origin[i+3:], r.Host)
}

// ServeHTTP is the Engine.IO endpoint for both transports.
func (h *Hub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !h.originAllowed(r) {
		writeEngineError(w, http.StatusForbidden, 4, "Forbidden")
		return
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		hd := w.Header()
		hd.Set("Access-Control-Allow-Origin", origin)
		hd.Set("Access-Control-Allow-Credentials", "true")
		hd.Add("Vary", "Origin")
		if r.Method == http.MethodOptions {
			hd.Set("Access-Control-Allow-Methods", "GET,HEAD,PUT,PATCH,POST,DELETE")
			if req := r.Header.Get("Access-Control-Request-Headers"); req != "" {
				hd.Set("Access-Control-Allow-Headers", req)
				hd.Add("Vary", "Access-Control-Request-Headers")
			}
			hd.Set("Content-Length", "0")
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}
	q := r.URL.Query()
	if q.Get("EIO") != "4" {
		writeEngineError(w, http.StatusBadRequest, 5, "Unsupported protocol version")
		return
	}
	transport, sid := q.Get("transport"), q.Get("sid")
	if transport != transportPolling && transport != transportWebsocket {
		writeEngineError(w, http.StatusBadRequest, 0, "Transport unknown")
		return
	}
	if sid == "" {
		h.handshake(w, r, transport)
		return
	}
	c := h.conn(sid)
	if c == nil {
		writeEngineError(w, http.StatusBadRequest, 1, "Session ID unknown")
		return
	}
	switch {
	case transport == transportWebsocket:
		ws, err := acceptWebsocket(w, r)
		if err != nil {
			return
		}
		c.upgrade(ws)
	case r.Method == http.MethodGet:
		c.poll(w, r)
	case r.Method == http.MethodPost:
		c.receivePost(w, r)
	default:
		writeEngineError(w, http.StatusBadRequest, 3, "Bad request")
	}
}

func acceptWebsocket(w http.ResponseWriter, r *http.Request) (*websocket.Conn, error) {
	// The origin was checked against the hub's policy already.
	return websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true,
		CompressionMode:    websocket.CompressionDisabled,
	})
}

func (h *Hub) handshake(w http.ResponseWriter, r *http.Request, transport string) {
	if r.Method != http.MethodGet {
		writeEngineError(w, http.StatusBadRequest, 2, "Bad handshake method")
		return
	}
	if transport == transportWebsocket {
		ws, err := acceptWebsocket(w, r)
		if err != nil {
			return
		}
		c, err := h.newConn(r, transport)
		if err != nil {
			_ = ws.Close(websocket.StatusTryAgainLater, "server unavailable")
			return
		}
		c.serveWebsocket(ws)
		return
	}
	c, err := h.newConn(r, transport)
	if err != nil {
		writeEngineError(w, http.StatusServiceUnavailable, 3, "Server unavailable")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=UTF-8")
	_, _ = w.Write([]byte(encodePollingPayload([]outPacket{c.openPacket()})))
}
