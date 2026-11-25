// Package wsgate holds what the raw WebSocket gateways of an app have in
// common (wearable, desktop companion, computer display, ...): admission of
// an upgrade request (origin policy, per-address attempt limit, signed
// session) and a connection with size limits and a ping keepalive.
//
// A gateway calls Admit, does its own checks, then Accept:
//
//	userID, ok := gate.Admit(w, r)
//	if !ok { return } // the error response is written
//	conn, err := gate.Accept(w, r)
//
// The gateway protocols stay in the apps; only the shared plumbing is here.
package wsgate

import (
	"net/http"
	"sync"
	"time"

	"github.com/NeoLabs-Systems/neolabs-go/httpx"
	"github.com/NeoLabs-Systems/neolabs-go/sessionx"
	"github.com/coder/websocket"
)

// Options configure a Gate.
type Options struct {
	// AllowOrigin decides which browser origins may upgrade; it is called with
	// the empty string when the request has no Origin header (native clients).
	// Nil allows requests without an Origin and from the server's own host.
	AllowOrigin func(origin string) bool
	// MaxMessage is the largest message a client may send.
	MaxMessage int64
	// UpgradeWindow and UpgradeMax limit upgrade attempts per client address;
	// defaults 1 minute and 30, the limits of the Node gateways.
	UpgradeWindow time.Duration
	UpgradeMax    int
	// TrustProxy reads the client address from the proxy's X-Forwarded-For.
	TrustProxy bool
	// SessionKey is the session value that holds the account id, "userId".
	SessionKey string
}

// Gate admits and accepts WebSocket upgrades for one endpoint.
type Gate struct {
	opts    Options
	limiter *attemptLimiter

	mu      sync.Mutex
	closing bool
	conns   map[*Conn]struct{}
}

// New creates a Gate.
func New(opts Options) *Gate {
	if opts.MaxMessage <= 0 {
		opts.MaxMessage = 1 << 20
	}
	if opts.UpgradeWindow <= 0 {
		opts.UpgradeWindow = time.Minute
	}
	if opts.UpgradeMax <= 0 {
		opts.UpgradeMax = 30
	}
	if opts.SessionKey == "" {
		opts.SessionKey = "userId"
	}
	return &Gate{
		opts:    opts,
		limiter: newAttemptLimiter(opts.UpgradeWindow, opts.UpgradeMax),
		conns:   map[*Conn]struct{}{},
	}
}

// reject answers like the Node gateways: a bare status line, no body.
func reject(w http.ResponseWriter, status int) {
	w.Header().Set("Connection", "close")
	w.WriteHeader(status)
}

// Admit runs the checks every gateway shares, in the order of the Node
// gateways: shutdown (503), attempt limit (429), origin (403), session (401).
// It returns the signed-in account. On failure it has already responded.
func (g *Gate) Admit(w http.ResponseWriter, r *http.Request) (userID int64, ok bool) {
	g.mu.Lock()
	closing := g.closing
	g.mu.Unlock()
	if closing {
		reject(w, http.StatusServiceUnavailable)
		return 0, false
	}
	if !g.limiter.allow(httpx.ClientIP(r, g.opts.TrustProxy)) {
		reject(w, http.StatusTooManyRequests)
		return 0, false
	}
	if !g.originAllowed(r) {
		reject(w, http.StatusForbidden)
		return 0, false
	}
	sess := sessionx.FromContext(r.Context())
	if sess == nil {
		reject(w, http.StatusInternalServerError)
		return 0, false
	}
	id, signedIn := sess.Int64(g.opts.SessionKey)
	if !signedIn || id == 0 {
		reject(w, http.StatusUnauthorized)
		return 0, false
	}
	return id, true
}

func (g *Gate) originAllowed(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if g.opts.AllowOrigin != nil {
		return g.opts.AllowOrigin(origin)
	}
	return origin == "" || sameHost(origin, r.Host)
}

// Accept completes the WebSocket handshake. The caller has admitted the request.
// A nil error means the response is taken over by the connection.
func (g *Gate) Accept(w http.ResponseWriter, r *http.Request) (*Conn, error) {
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true, // Admit applied the origin policy
		CompressionMode:    websocket.CompressionDisabled,
	})
	if err != nil {
		return nil, err
	}
	ws.SetReadLimit(g.opts.MaxMessage)
	c := &Conn{ws: ws, gate: g, remote: httpx.ClientIP(r, g.opts.TrustProxy)}
	g.mu.Lock()
	if g.closing {
		g.mu.Unlock()
		_ = ws.Close(websocket.StatusGoingAway, "server shutting down")
		return nil, ErrClosed
	}
	g.conns[c] = struct{}{}
	g.mu.Unlock()
	return c, nil
}

// Close refuses new upgrades and drops every connection of the gate.
func (g *Gate) Close() {
	g.mu.Lock()
	g.closing = true
	conns := make([]*Conn, 0, len(g.conns))
	for c := range g.conns {
		conns = append(conns, c)
	}
	g.mu.Unlock()
	for _, c := range conns {
		c.Terminate()
	}
}

func (g *Gate) forget(c *Conn) {
	g.mu.Lock()
	delete(g.conns, c)
	g.mu.Unlock()
}
