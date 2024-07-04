package websocket

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"

	"github.com/gorilla/websocket"
)

const defaultReadLimit = 32 * 1024 * 1024

type ConnectionMetadata struct {
	RemoteAddr string
	Origin     string
	UserAgent  string
	Context    context.Context
}

type OnConnectFn[E any] func(c *Connection, meta *ConnectionMetadata) (E, error)

type OnDisconnectFn[E any] func(e E)

type Server[E any] struct {
	// *Connection -> E
	connections sync.Map

	count atomic.Int64

	onConnect OnConnectFn[E]
	conf      serverConfig[E]
}

type serverConfig[E any] struct {
	onDisconnect OnDisconnectFn[E]

	readLimit int64

	checkOrigin func(r *http.Request) bool

	onUpgradeFailed func(r *http.Request, err error)
}

type ServerOpt[E any] func(c *serverConfig[E])

func WithOnDisconnect[E any](onDisconnect OnDisconnectFn[E]) ServerOpt[E] {
	return func(c *serverConfig[E]) {
		c.onDisconnect = onDisconnect
	}
}

func WithReadLimit[E any](readLimit int64) ServerOpt[E] {
	return func(c *serverConfig[E]) {
		c.readLimit = readLimit
	}
}

func WithCheckOrigin[E any](fn func(r *http.Request) bool) ServerOpt[E] {
	return func(c *serverConfig[E]) {
		c.checkOrigin = fn
	}
}

func WithOnUpgradeFailed[E any](fn func(r *http.Request, err error)) ServerOpt[E] {
	return func(c *serverConfig[E]) {
		c.onUpgradeFailed = fn
	}
}

func NewServer[E any](onConnect OnConnectFn[E], opts ...ServerOpt[E]) *Server[E] {
	srv := &Server[E]{
		onConnect: onConnect,
		conf: serverConfig[E]{
			onDisconnect: nil,
			readLimit:    defaultReadLimit,
		},
	}
	for _, fn := range opts {
		fn(&srv.conf)
	}
	return srv
}

func (s *Server[E]) Handle(w http.ResponseWriter, r *http.Request) {
	upgrader := websocket.Upgrader{
		ReadBufferSize:  readBuffer,
		WriteBufferSize: writeBuffer,
		WriteBufferPool: bufferPool,
		CheckOrigin:     s.conf.checkOrigin,
	}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		if s.conf.onUpgradeFailed != nil {
			s.conf.onUpgradeFailed(r, err)
		}
		return
	}
	conn.SetReadLimit(s.conf.readLimit)
	metadata := &ConnectionMetadata{
		RemoteAddr: r.RemoteAddr,
		Origin:     r.Header.Get("Origin"),
		UserAgent:  r.Header.Get("User-Agent"),
		Context:    r.Context(),
	}
	wc := setupConnection(conn)
	connData, err := s.onConnect(wc, metadata)
	if err != nil {
		wc.CloseWithCause(err)
		return
	}
	s.connections.Store(wc, connData)
	defer s.connections.Delete(wc)
	s.count.Add(1)
	defer s.count.Add(-1)
	if s.conf.onDisconnect != nil {
		defer s.conf.onDisconnect(connData)
	}
	closeCtx := wc.CloseCtx()
	// wait for connection to be closed
	<-closeCtx.Done()
}

func (s *Server[E]) Range(fn func(e E) bool) {
	s.connections.Range(func(key, value any) bool {
		return fn(value.(E))
	})
}

func (s *Server[E]) Count() int64 {
	return s.count.Load()
}
