package realtime

import (
	"errors"
	"net/http"
	"sync"
	"time"
)

// ErrAckTimeout is passed to an EmitWithAck callback when the client did not
// answer in time or disconnected first.
var ErrAckTimeout = errors.New("realtime: acknowledgement timed out")

// Event is a client-to-server event.
type Event struct {
	Socket *Socket
	Name   string
	Args   []any // decoded JSON; binary attachments are []byte
	// Ack answers the client's acknowledgement callback. It does nothing when
	// the client did not ask for one, and only the first call counts.
	Ack func(args ...any)
}

// Arg returns argument i, or nil.
func (e *Event) Arg(i int) any {
	if i < 0 || i >= len(e.Args) {
		return nil
	}
	return e.Args[i]
}

// Object returns argument i when it is a JSON object, otherwise an empty map.
func (e *Event) Object(i int) map[string]any {
	if m, ok := e.Arg(i).(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

// Handler handles one client event. Handlers of a client run one after
// another in arrival order, so a handler that waits for long work should start
// a goroutine for it.
type Handler func(e *Event)

type pendingAck struct {
	cb    func(args []any, err error)
	timer *time.Timer
}

// Socket is one client's connection to one namespace.
type Socket struct {
	ns *Namespace
	c  *conn
	id string

	// Data is free storage for the application, like socket.data in Node.
	Data sync.Map

	mu           sync.Mutex
	handlers     map[string]Handler
	onDisconnect []func(reason string)
	rooms        map[string]struct{}
	acks         map[uint64]*pendingAck
	nextAck      uint64
	gone         bool
}

func newSocket(ns *Namespace, c *conn, id string) *Socket {
	return &Socket{
		ns: ns, c: c, id: id,
		handlers: map[string]Handler{},
		rooms:    map[string]struct{}{},
		acks:     map[uint64]*pendingAck{},
	}
}

// ID is the socket's id, also the name of the room that holds only this socket.
func (s *Socket) ID() string { return s.id }

// Request is the HTTP request that opened the connection, with the values the
// middleware chain attached to its context (such as the session). Its context
// is never cancelled.
func (s *Socket) Request() *http.Request { return s.c.req }

// On registers the handler for an event name, replacing an earlier one.
func (s *Socket) On(event string, h Handler) {
	s.mu.Lock()
	s.handlers[event] = h
	s.mu.Unlock()
}

// OnDisconnect registers a function called once when the socket is gone, with
// the Socket.IO reason ("transport close", "ping timeout", "client namespace
// disconnect", ...).
func (s *Socket) OnDisconnect(fn func(reason string)) {
	s.mu.Lock()
	s.onDisconnect = append(s.onDisconnect, fn)
	s.mu.Unlock()
}

// Join adds the socket to rooms.
func (s *Socket) Join(rooms ...string) {
	s.mu.Lock()
	if s.gone {
		s.mu.Unlock()
		return
	}
	for _, r := range rooms {
		s.rooms[r] = struct{}{}
	}
	s.mu.Unlock()
	for _, r := range rooms {
		s.ns.join(r, s)
	}
	s.mu.Lock()
	gone := s.gone
	s.mu.Unlock()
	if gone { // disconnected while joining: do not leave a stale member behind
		s.ns.remove(s)
	}
}

// Leave removes the socket from a room.
func (s *Socket) Leave(room string) {
	s.mu.Lock()
	delete(s.rooms, room)
	s.mu.Unlock()
	s.ns.leave(room, s)
}

func (s *Socket) roomSet() map[string]struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]struct{}, len(s.rooms))
	for r := range s.rooms {
		out[r] = struct{}{}
	}
	return out
}

// To addresses the other sockets of the namespace in the given rooms.
func (s *Socket) To(rooms ...string) Broadcast {
	return s.ns.To(rooms...).Except(s.id)
}

// Emit sends an event to this client. []byte arguments travel as binary
// attachments.
func (s *Socket) Emit(event string, args ...any) error {
	return s.emit(event, false, args)
}

// EmitVolatile is Emit, but the event is dropped if the client is not keeping up.
func (s *Socket) EmitVolatile(event string, args ...any) error {
	return s.emit(event, true, args)
}

func (s *Socket) emit(event string, volatile bool, args []any) error {
	pkts, err := encodeMessage(s.ns.name, event, nil, args)
	if err != nil {
		return err
	}
	s.c.enqueue(pkts, volatile)
	return nil
}

// EmitWithAck sends an event and calls cb with the client's answer, or with
// ErrAckTimeout. cb runs on its own goroutine.
func (s *Socket) EmitWithAck(event string, timeout time.Duration, cb func(args []any, err error), args ...any) error {
	s.mu.Lock()
	if s.gone {
		s.mu.Unlock()
		return errors.New("realtime: socket is disconnected")
	}
	s.nextAck++
	id := s.nextAck
	p := &pendingAck{cb: cb}
	p.timer = time.AfterFunc(timeout, func() { s.finishAck(id, nil, ErrAckTimeout) })
	s.acks[id] = p
	s.mu.Unlock()
	pkts, err := encodeMessage(s.ns.name, event, &id, args)
	if err != nil {
		s.finishAck(id, nil, err)
		return err
	}
	s.c.enqueue(pkts, false)
	return nil
}

func (s *Socket) finishAck(id uint64, args []any, err error) {
	s.mu.Lock()
	p := s.acks[id]
	delete(s.acks, id)
	s.mu.Unlock()
	if p == nil {
		return
	}
	p.timer.Stop()
	go p.cb(args, err)
}

func (s *Socket) receiveAck(p sioPacket, args []any) {
	if p.hasID {
		s.finishAck(p.id, args, nil)
	}
}

func (s *Socket) receiveEvent(p sioPacket, args []any) {
	name, _ := args[0].(string)
	s.mu.Lock()
	h := s.handlers[name]
	s.mu.Unlock()
	if h == nil {
		return
	}
	ev := &Event{Socket: s, Name: name, Args: args[1:], Ack: func(...any) {}}
	if p.hasID {
		var once sync.Once
		id := p.id
		ev.Ack = func(out ...any) {
			once.Do(func() {
				pkts, err := encodeMessage(s.ns.name, "", &id, out)
				if err == nil {
					s.c.enqueue(pkts, false)
				}
			})
		}
	}
	h(ev)
}

// Disconnect tells the client it was disconnected from the namespace. With
// closeConn the whole connection is closed once the queue is flushed.
func (s *Socket) Disconnect(closeConn bool) {
	if !s.markGone() {
		return
	}
	s.c.mu.Lock()
	delete(s.c.sockets, s.ns.name)
	s.c.mu.Unlock()
	s.ns.remove(s)
	s.c.enqueue([]outPacket{{kind: eioMessage, text: encodeSIO(sioDisconnect, s.ns.name, nil, 0, nil)}}, false)
	go s.runDisconnect("server namespace disconnect")
	if closeConn {
		s.c.shutdown()
	}
}

func (s *Socket) disconnectByClient() {
	if !s.markGone() {
		return
	}
	s.c.mu.Lock()
	delete(s.c.sockets, s.ns.name)
	s.c.mu.Unlock()
	s.ns.remove(s)
	s.runDisconnect("client namespace disconnect")
}

// markGone flips the socket to disconnected exactly once.
func (s *Socket) markGone() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.gone {
		return false
	}
	s.gone = true
	return true
}

// fireDisconnect is the path of a closed connection.
func (s *Socket) fireDisconnect(reason string) {
	if s.markGone() {
		s.runDisconnect(reason)
	}
}

func (s *Socket) runDisconnect(reason string) {
	s.mu.Lock()
	fns := s.onDisconnect
	s.onDisconnect = nil
	pending := s.acks
	s.acks = map[uint64]*pendingAck{}
	s.mu.Unlock()
	for _, p := range pending {
		p.timer.Stop()
		go p.cb(nil, ErrAckTimeout)
	}
	for _, fn := range fns {
		fn(reason)
	}
}
