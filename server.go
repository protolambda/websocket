package websocket

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"sync"

	"github.com/gorilla/websocket"
)

// ErrServerClosed is the cause of connections closed by Server.Close.
var ErrServerClosed = errors.New("websocket server closed")

// errServerClosing wraps context.Canceled, so the connections are closed gracefully, with a close message.
var errServerClosing = fmt.Errorf("%w: %w", ErrServerClosed, context.Canceled)

// ConnectionMetadata describes the request that opened a connection.
type ConnectionMetadata struct {
	RemoteAddr string
	Origin     string
	UserAgent  string
	// Header holds the headers of the upgrade request.
	// It may contain credentials, e.g. Authorization or Cookie.
	Header http.Header
	// Context is the context of the upgrade request. It is done when Handle returns.
	Context context.Context
}

// OnConnectFn is called when a connection opens, before it is registered with the Server.
// An error closes the connection, without a call to OnDisconnectFn.
type OnConnectFn[E any] func(c *Connection, meta *ConnectionMetadata) (E, error)

// OnDisconnectFn is called after a connection closed, with the value of its OnConnectFn call.
type OnDisconnectFn[E any] func(e E)

// Server accepts websocket connections, and maintains the set of open connections, with a value E for each.
type Server[E any] struct {
	onConnect OnConnectFn[E]
	conf      serverConfig[E]
	upgrader  websocket.Upgrader

	mu          sync.Mutex
	connections map[*Connection]E
	closed      bool
	// handlers tracks the Handle calls, for Close to wait on.
	handlers sync.WaitGroup
}

type serverConfig[E any] struct {
	onDisconnect OnDisconnectFn[E]

	checkOrigin func(r *http.Request) bool

	onUpgradeFailed func(r *http.Request, err error)

	conn connConfig
}

// ServerOpt configures a Server.
type ServerOpt[E any] func(c *serverConfig[E])

// WithOnDisconnect sets the function that is called after a connection closed.
func WithOnDisconnect[E any](onDisconnect OnDisconnectFn[E]) ServerOpt[E] {
	return func(c *serverConfig[E]) {
		c.onDisconnect = onDisconnect
	}
}

// WithCheckOrigin sets the function that decides whether to accept an upgrade request.
// By default, the Origin header must be absent, or match the Host header.
func WithCheckOrigin[E any](fn func(r *http.Request) bool) ServerOpt[E] {
	return func(c *serverConfig[E]) {
		c.checkOrigin = fn
	}
}

// WithOnUpgradeFailed sets the function that is called when a request fails to upgrade to a websocket connection.
// The HTTP error response is already written when it is called.
func WithOnUpgradeFailed[E any](fn func(r *http.Request, err error)) ServerOpt[E] {
	return func(c *serverConfig[E]) {
		c.onUpgradeFailed = fn
	}
}

// WithConnOpts applies connection options to the connections of the Server, e.g. WithReadLimit.
func WithConnOpts[E any](opts ...ConnOpt) ServerOpt[E] {
	return func(c *serverConfig[E]) {
		for _, opt := range opts {
			opt(&c.conn)
		}
	}
}

// NewServer creates a Server. Its Handle method serves the connections.
func NewServer[E any](onConnect OnConnectFn[E], opts ...ServerOpt[E]) *Server[E] {
	srv := &Server[E]{
		onConnect: onConnect,
		conf: serverConfig[E]{
			onDisconnect: nil,
			conn:         defaultConnConfig(),
		},
		connections: make(map[*Connection]E),
	}
	for _, fn := range opts {
		fn(&srv.conf)
	}
	srv.upgrader = websocket.Upgrader{
		ReadBufferSize:    readBuffer,
		WriteBufferSize:   writeBuffer,
		WriteBufferPool:   bufferPool,
		CheckOrigin:       srv.conf.checkOrigin,
		EnableCompression: srv.conf.conn.compression,
	}
	return srv
}

// Handle upgrades the request to a websocket connection, and serves it until it closes.
// It calls OnConnect, registers the connection, waits for it to close, and then calls OnDisconnect.
// After Close, it answers 503 Service Unavailable.
func (s *Server[E]) Handle(w http.ResponseWriter, r *http.Request) {
	if !s.enter() {
		http.Error(w, ErrServerClosed.Error(), http.StatusServiceUnavailable)
		return
	}
	defer s.handlers.Done()
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		if s.conf.onUpgradeFailed != nil {
			s.conf.onUpgradeFailed(r, err)
		}
		return
	}
	metadata := &ConnectionMetadata{
		RemoteAddr: r.RemoteAddr,
		Origin:     r.Header.Get("Origin"),
		UserAgent:  r.Header.Get("User-Agent"),
		Header:     r.Header.Clone(),
		Context:    r.Context(),
	}
	wc := newConnection(conn, s.conf.conn)
	connData, err := s.onConnect(wc, metadata)
	if err != nil {
		wc.CloseWithCause(err)
		return
	}
	if s.conf.onDisconnect != nil {
		defer s.conf.onDisconnect(connData)
	}
	if !s.register(wc, connData) {
		// Close was called while OnConnect ran.
		wc.CloseWithCause(errServerClosing)
		return
	}
	defer s.unregister(wc)
	// wait for connection to be closed
	<-wc.CloseCtx().Done()
}

// enter registers a Handle call, unless the server is closed.
func (s *Server[E]) enter() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	s.handlers.Add(1)
	return true
}

func (s *Server[E]) register(c *Connection, e E) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	s.connections[c] = e
	return true
}

func (s *Server[E]) unregister(c *Connection) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.connections, c)
}

// Range calls fn for each open connection, in no particular order, until fn returns false.
// It iterates over a snapshot, without holding a lock: fn may call other methods of the Server.
func (s *Server[E]) Range(fn func(e E) bool) {
	s.mu.Lock()
	values := slices.Collect(maps.Values(s.connections))
	s.mu.Unlock()
	for _, e := range values {
		if !fn(e) {
			return
		}
	}
}

// Count returns the number of open connections.
func (s *Server[E]) Count() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return int64(len(s.connections))
}

// Close stops accepting connections, and closes the open connections gracefully, with a cause that wraps
// ErrServerClosed. It waits until every Handle call returned, so until OnDisconnect ran for every connection.
// It must not be called from OnConnect or OnDisconnect, since it waits for them.
func (s *Server[E]) Close() {
	s.mu.Lock()
	s.closed = true
	conns := slices.Collect(maps.Keys(s.connections))
	s.mu.Unlock()
	// Concurrently: each close may wait for the close timeout, if the peer does not read.
	for _, c := range conns {
		go c.CloseWithCause(errServerClosing)
	}
	s.handlers.Wait()
}
