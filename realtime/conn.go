package realtime

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
)

const (
	transportPolling   = "polling"
	transportWebsocket = "websocket"

	volatileBacklog = 8 // a volatile packet is dropped once this many wait for the client
	inboxSize       = 512
	upgradeTimeout  = 10 * time.Second
)

// conn is one Engine.IO session: a client connection that can carry several
// Socket.IO namespaces and moves between the polling and websocket transports.
type conn struct {
	h   *Hub
	id  string
	req *http.Request // the handshake request, detached from its lifetime

	mu        sync.Mutex
	queue     []outPacket
	transport string
	upgrading bool
	polling   bool // a long poll GET is waiting for packets
	closing   bool // flush what is queued, then close
	closed    bool
	ws        *websocket.Conn
	probe     *websocket.Conn // upgrade candidate until the client confirms
	sockets   map[string]*Socket

	notify   chan struct{} // new packets queued
	activity chan struct{} // the client sent something
	done     chan struct{}
	inbox    chan inbound

	inMu       sync.Mutex // guards the attachment assembly below
	pending    *sioPacket
	pendingBin [][]byte
}

type inbound struct {
	pkt  sioPacket
	args []any
}

func (h *Hub) newConn(r *http.Request, transport string) (*conn, error) {
	id, err := randomID()
	if err != nil {
		return nil, err
	}
	c := &conn{
		h:         h,
		id:        id,
		req:       r.Clone(context.WithoutCancel(r.Context())),
		transport: transport,
		sockets:   map[string]*Socket{},
		notify:    make(chan struct{}, 1),
		activity:  make(chan struct{}, 1),
		done:      make(chan struct{}),
		inbox:     make(chan inbound, inboxSize),
	}
	if !h.register(c) {
		return nil, errHubClosed
	}
	go c.dispatchLoop()
	go c.pingLoop()
	time.AfterFunc(h.opts.ConnectTimeout, func() {
		c.mu.Lock()
		idle := len(c.sockets) == 0
		c.mu.Unlock()
		if idle {
			c.close("forced close")
		}
	})
	return c, nil
}

func (c *conn) openPacket() outPacket {
	upgrades := "[]"
	if c.transport == transportPolling {
		upgrades = `["websocket"]`
	}
	o := c.h.opts
	return outPacket{kind: eioOpen, text: fmt.Sprintf(
		`{"sid":%q,"upgrades":%s,"pingInterval":%d,"pingTimeout":%d,"maxPayload":%d}`,
		c.id, upgrades, o.PingInterval.Milliseconds(), o.PingTimeout.Milliseconds(), o.MaxPayload)}
}

// enqueue queues packets for the client. They stay together so that a binary
// event's attachments follow their head.
func (c *conn) enqueue(pkts []outPacket, volatile bool) {
	c.mu.Lock()
	if c.closed || c.closing {
		c.mu.Unlock()
		return
	}
	if volatile && len(c.queue) > volatileBacklog {
		c.mu.Unlock()
		return
	}
	if len(c.queue)+len(pkts) > c.h.opts.MaxQueuedPackets {
		c.mu.Unlock()
		c.close("buffer overflow")
		return
	}
	c.queue = append(c.queue, pkts...)
	c.mu.Unlock()
	c.signal()
}

func (c *conn) signal() {
	select {
	case c.notify <- struct{}{}:
	default:
	}
}

func (c *conn) requeueFront(pkts []outPacket) {
	c.mu.Lock()
	c.queue = append(append([]outPacket(nil), pkts...), c.queue...)
	c.mu.Unlock()
	c.signal()
}

func (c *conn) takeQueue() []outPacket {
	c.mu.Lock()
	defer c.mu.Unlock()
	q := c.queue
	c.queue = nil
	return q
}

// shutdown flushes the queue, then closes the session.
func (c *conn) shutdown() {
	c.mu.Lock()
	if c.closed || c.closing {
		c.mu.Unlock()
		return
	}
	c.closing = true
	c.queue = append(c.queue, outPacket{kind: eioClose})
	c.mu.Unlock()
	c.signal()
	time.AfterFunc(c.h.opts.PingTimeout, func() { c.close("forced close") })
}

// close ends the session at once and disconnects its sockets.
func (c *conn) close(reason string) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	close(c.done)
	ws, probe := c.ws, c.probe
	socks := make([]*Socket, 0, len(c.sockets))
	for _, s := range c.sockets {
		socks = append(socks, s)
	}
	c.sockets = map[string]*Socket{}
	c.mu.Unlock()

	c.h.unregister(c)
	if ws != nil {
		_ = ws.CloseNow()
	}
	if probe != nil {
		_ = probe.CloseNow()
	}
	for _, s := range socks {
		s.ns.remove(s)
		go s.fireDisconnect(reason)
	}
}

// pingLoop sends the Engine.IO v4 heartbeat and closes the session when the
// client stops answering.
func (c *conn) pingLoop() {
	o := c.h.opts
	timer := time.NewTimer(o.PingInterval)
	defer timer.Stop()
	for {
		select {
		case <-c.done:
			return
		case <-timer.C:
		}
		select { // forget activity from before this ping
		case <-c.activity:
		default:
		}
		c.enqueue([]outPacket{{kind: eioPing}}, false)
		select {
		case <-c.done:
			return
		case <-c.activity:
		case <-time.After(o.PingTimeout):
			c.close("ping timeout")
			return
		}
		timer.Reset(o.PingInterval)
	}
}

func (c *conn) touch() {
	select {
	case c.activity <- struct{}{}:
	default:
	}
}

// onPacket handles one Engine.IO packet from either transport.
func (c *conn) onPacket(p inPacket) {
	c.touch()
	switch {
	case p.isBin:
		c.onMessage(true, "", p.bin)
	case p.kind == eioMessage:
		c.onMessage(false, p.text, nil)
	case p.kind == eioPing:
		c.enqueue([]outPacket{{kind: eioPong, text: p.text}}, false)
	case p.kind == eioClose:
		c.close("transport close")
	}
}

// onMessage assembles Socket.IO packets, including their binary attachments.
func (c *conn) onMessage(isBin bool, text string, bin []byte) {
	c.inMu.Lock()
	defer c.inMu.Unlock()
	if isBin {
		if c.pending == nil {
			c.close("parse error")
			return
		}
		c.pendingBin = append(c.pendingBin, bin)
		if len(c.pendingBin) == c.pending.attachments {
			p, bins := *c.pending, c.pendingBin
			c.pending, c.pendingBin = nil, nil
			c.deliver(p, bins)
		}
		return
	}
	if c.pending != nil {
		c.close("parse error")
		return
	}
	p, err := decodeSIO(text)
	if err != nil {
		c.close("parse error")
		return
	}
	if p.isBinary() && p.attachments > 0 {
		c.pending = &p
		return
	}
	c.deliver(p, nil)
}

func (c *conn) deliver(p sioPacket, bins [][]byte) {
	in := inbound{pkt: p}
	if p.typ == sioEvent || p.typ == sioBinaryEvent || p.typ == sioAck || p.typ == sioBinaryAck {
		args, err := p.args(bins)
		if err != nil {
			c.close("parse error")
			return
		}
		in.args = args
	}
	select {
	case c.inbox <- in:
	default:
		c.close("buffer overflow")
	}
}

// dispatchLoop runs connects and event handlers one after another, so a
// client's events are handled in the order it sent them.
func (c *conn) dispatchLoop() {
	for {
		select {
		case <-c.done:
			return
		case in := <-c.inbox:
			c.dispatch(in)
		}
	}
}

func (c *conn) dispatch(in inbound) {
	p := in.pkt
	if p.typ == sioConnect {
		c.connectNamespace(p)
		return
	}
	c.mu.Lock()
	s := c.sockets[p.nsp]
	c.mu.Unlock()
	if s == nil {
		return
	}
	switch p.typ {
	case sioDisconnect:
		s.disconnectByClient()
	case sioEvent, sioBinaryEvent:
		s.receiveEvent(p, in.args)
	case sioAck, sioBinaryAck:
		s.receiveAck(p, in.args)
	}
}

func (c *conn) connectNamespace(p sioPacket) {
	ns := c.h.lookup(p.nsp)
	if ns == nil {
		c.sendConnectError(p.nsp, "Invalid namespace")
		return
	}
	c.mu.Lock()
	_, exists := c.sockets[p.nsp]
	c.mu.Unlock()
	if exists {
		return
	}
	var auth json.RawMessage
	if p.data != "" {
		auth = json.RawMessage(p.data)
	}
	ns.connect(c, auth)
}

func (c *conn) sendConnectError(nsp, message string) {
	data, _ := marshalJSON(map[string]string{"message": message})
	c.enqueue([]outPacket{{kind: eioMessage, text: encodeSIO(sioConnectError, nsp, nil, 0, data)}}, false)
}

// writeLoop drains the queue into a websocket until the session ends.
func (c *conn) writeLoop(ws *websocket.Conn) {
	for {
		select {
		case <-c.done:
			return
		case <-c.notify:
		}
		for _, p := range c.takeQueue() {
			if p.kind == eioClose && !p.isBin {
				_ = ws.Close(websocket.StatusNormalClosure, "")
				c.close("forced close")
				return
			}
			if err := c.writeWS(ws, p); err != nil {
				c.close("transport error")
				return
			}
		}
	}
}

func (c *conn) writeWS(ws *websocket.Conn, p outPacket) error {
	ctx, cancel := context.WithTimeout(context.Background(), c.h.opts.PingTimeout)
	defer cancel()
	if p.isBin {
		return ws.Write(ctx, websocket.MessageBinary, p.bin)
	}
	return ws.Write(ctx, websocket.MessageText, append([]byte{p.kind}, p.text...))
}

// readLoop feeds websocket frames into the session until the socket ends.
func (c *conn) readLoop(ws *websocket.Conn) {
	for {
		typ, data, err := ws.Read(context.Background())
		if err != nil {
			c.close("transport close")
			return
		}
		if typ == websocket.MessageBinary {
			c.onPacket(inPacket{kind: eioMessage, bin: data, isBin: true})
			continue
		}
		if len(data) == 0 || data[0] < eioOpen || data[0] > eioNoop {
			c.close("parse error")
			return
		}
		c.onPacket(inPacket{kind: data[0], text: string(data[1:])})
	}
}

// serveWebsocket runs a session whose first transport is a websocket.
func (c *conn) serveWebsocket(ws *websocket.Conn) {
	ws.SetReadLimit(c.h.opts.MaxPayload)
	c.mu.Lock()
	c.ws = ws
	c.mu.Unlock()
	if err := c.writeWS(ws, c.openPacket()); err != nil {
		c.close("transport error")
		return
	}
	go c.writeLoop(ws)
	c.readLoop(ws)
}

// upgrade moves a polling session to a websocket: the client probes the new
// transport, the server answers, and the client confirms with an upgrade packet.
func (c *conn) upgrade(ws *websocket.Conn) {
	ws.SetReadLimit(c.h.opts.MaxPayload)
	c.mu.Lock()
	if c.closed || c.upgrading || c.transport != transportPolling {
		c.mu.Unlock()
		_ = ws.Close(websocket.StatusPolicyViolation, "bad request")
		return
	}
	c.upgrading = true
	c.probe = ws
	c.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), upgradeTimeout)
	defer cancel()
	if !c.handleProbe(ctx, ws) {
		c.mu.Lock()
		c.upgrading, c.probe = false, nil
		c.mu.Unlock()
		_ = ws.CloseNow()
		return
	}
	c.mu.Lock()
	c.transport, c.ws, c.probe, c.upgrading = transportWebsocket, ws, nil, false
	c.mu.Unlock()
	go c.writeLoop(ws)
	c.signal() // packets queued during the upgrade go out on the new transport
	c.readLoop(ws)
}

func (c *conn) handleProbe(ctx context.Context, ws *websocket.Conn) bool {
	_, data, err := ws.Read(ctx)
	if err != nil || string(data) != "2probe" {
		return false
	}
	if err := ws.Write(ctx, websocket.MessageText, []byte("3probe")); err != nil {
		return false
	}
	c.mu.Lock()
	waiting := c.polling
	c.mu.Unlock()
	if waiting { // release the pending poll so the client moves on
		c.mu.Lock()
		c.queue = append(c.queue, outPacket{kind: eioNoop})
		c.mu.Unlock()
		c.signal()
	}
	_, data, err = ws.Read(ctx)
	return err == nil && string(data) == "5"
}

// poll answers a long poll GET with the queued packets.
func (c *conn) poll(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	switch {
	case c.closed || c.transport != transportPolling:
		c.mu.Unlock()
		writeEngineError(w, http.StatusBadRequest, 3, "Bad request")
		return
	case c.polling: // two overlapping polls are a client bug
		c.mu.Unlock()
		writeEngineError(w, http.StatusBadRequest, 3, "Bad request")
		c.close("transport error")
		return
	}
	c.polling = true
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.polling = false
		c.mu.Unlock()
	}()

	for {
		if pkts := c.takeQueue(); len(pkts) > 0 {
			// The close packet travels alone, after the client has had the
			// chance to handle what came before it.
			sendClose := false
			for i, p := range pkts {
				if p.kind == eioClose && !p.isBin {
					if i == 0 {
						sendClose = true
						break
					}
					c.requeueFront(pkts[i:])
					pkts = pkts[:i]
					break
				}
			}
			w.Header().Set("Content-Type", "text/plain; charset=UTF-8")
			if sendClose {
				pkts = pkts[:1]
			}
			_, _ = io.WriteString(w, encodePollingPayload(pkts))
			if sendClose {
				c.close("forced close")
			}
			return
		}
		select {
		case <-c.notify:
		case <-c.done:
			w.Header().Set("Content-Type", "text/plain; charset=UTF-8")
			_, _ = io.WriteString(w, string(rune(eioClose)))
			return
		case <-r.Context().Done():
			return
		}
	}
}

// receivePost handles a polling POST carrying packets from the client.
func (c *conn) receivePost(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	ok := !c.closed && c.transport == transportPolling
	c.mu.Unlock()
	if !ok {
		writeEngineError(w, http.StatusBadRequest, 3, "Bad request")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, c.h.opts.MaxPayload))
	if err != nil {
		writeEngineError(w, http.StatusRequestEntityTooLarge, 3, "Bad request")
		c.close("transport error")
		return
	}
	pkts, err := decodePollingPayload(string(body))
	if err != nil {
		writeEngineError(w, http.StatusBadRequest, 3, "Bad request")
		c.close("parse error")
		return
	}
	for _, p := range pkts {
		c.onPacket(p)
	}
	w.Header().Set("Content-Type", "text/html")
	_, _ = io.WriteString(w, "ok")
}
