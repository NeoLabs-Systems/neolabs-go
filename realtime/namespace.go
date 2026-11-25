package realtime

import (
	"encoding/json"
	"sync"
)

// Middleware runs when a client joins a namespace, before the connection
// handlers. A non-nil error refuses the client with a CONNECT_ERROR carrying
// the error text. auth is the raw `auth` object the client sent, or nil.
type Middleware func(s *Socket, auth json.RawMessage) error

// Namespace is one Socket.IO namespace with its rooms.
type Namespace struct {
	hub  *Hub
	name string

	mu      sync.RWMutex
	mws     []Middleware
	onConn  []func(*Socket)
	sockets map[string]*Socket
	rooms   map[string]map[string]*Socket
}

// Name is the namespace path, "/" for the default one.
func (n *Namespace) Name() string { return n.name }

// Use adds a connection middleware.
func (n *Namespace) Use(mw Middleware) {
	n.mu.Lock()
	n.mws = append(n.mws, mw)
	n.mu.Unlock()
}

// OnConnection registers a handler for every socket that joins. It runs after
// the client was told it is connected, and before the client's next event is
// handled, so it can register event handlers without missing one.
func (n *Namespace) OnConnection(fn func(*Socket)) {
	n.mu.Lock()
	n.onConn = append(n.onConn, fn)
	n.mu.Unlock()
}

func (n *Namespace) connect(c *conn, auth json.RawMessage) {
	id, err := randomID()
	if err != nil {
		c.sendConnectError(n.name, "Server error")
		return
	}
	s := newSocket(n, c, id)
	n.mu.RLock()
	mws := append([]Middleware(nil), n.mws...)
	handlers := append([]func(*Socket){}, n.onConn...)
	n.mu.RUnlock()
	for _, mw := range mws {
		if err := mw(s, auth); err != nil {
			c.sendConnectError(n.name, err.Error())
			return
		}
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.sockets[n.name] = s
	c.mu.Unlock()

	n.mu.Lock()
	n.sockets[id] = s
	n.mu.Unlock()
	s.Join(id)
	data, _ := marshalJSON(map[string]string{"sid": id})
	c.enqueue([]outPacket{{kind: eioMessage, text: encodeSIO(sioConnect, n.name, nil, 0, data)}}, false)
	for _, fn := range handlers {
		fn(s)
	}
}

// remove drops a socket and all its room memberships.
func (n *Namespace) remove(s *Socket) {
	n.mu.Lock()
	defer n.mu.Unlock()
	delete(n.sockets, s.id)
	for room := range s.roomSet() {
		n.leaveLocked(room, s)
	}
}

func (n *Namespace) join(room string, s *Socket) {
	n.mu.Lock()
	defer n.mu.Unlock()
	members := n.rooms[room]
	if members == nil {
		members = map[string]*Socket{}
		n.rooms[room] = members
	}
	members[s.id] = s
}

func (n *Namespace) leaveLocked(room string, s *Socket) {
	members := n.rooms[room]
	delete(members, s.id)
	if len(members) == 0 {
		delete(n.rooms, room)
	}
}

func (n *Namespace) leave(room string, s *Socket) {
	n.mu.Lock()
	n.leaveLocked(room, s)
	n.mu.Unlock()
}

// Sockets lists the sockets in a room.
func (n *Namespace) Sockets(room string) []*Socket {
	n.mu.RLock()
	defer n.mu.RUnlock()
	out := make([]*Socket, 0, len(n.rooms[room]))
	for _, s := range n.rooms[room] {
		out = append(out, s)
	}
	return out
}

// To addresses the sockets in the given rooms.
func (n *Namespace) To(rooms ...string) Broadcast { return Broadcast{ns: n, rooms: rooms} }

// Broadcast is a set of recipients and how to send to them.
type Broadcast struct {
	ns       *Namespace
	rooms    []string
	except   string
	volatile bool
}

// Volatile drops the event for clients that are not keeping up.
func (b Broadcast) Volatile() Broadcast { b.volatile = true; return b }

// Except leaves one socket out, such as the sender.
func (b Broadcast) Except(socketID string) Broadcast { b.except = socketID; return b }

// Emit sends an event to the recipients. []byte arguments travel as binary
// attachments.
func (b Broadcast) Emit(event string, args ...any) error {
	pkts, err := encodeMessage(b.ns.name, event, nil, args)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	b.ns.mu.RLock()
	var targets []*Socket
	for _, room := range b.rooms {
		for id, s := range b.ns.rooms[room] {
			if id != b.except && !seen[id] {
				seen[id] = true
				targets = append(targets, s)
			}
		}
	}
	b.ns.mu.RUnlock()
	for _, s := range targets {
		s.c.enqueue(pkts, b.volatile)
	}
	return nil
}
