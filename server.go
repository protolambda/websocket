package websocket

import (
	"net/http"
	"sync"
	"sync/atomic"

	"github.com/gorilla/websocket"
)

type ConnectionMetadata struct {
	RemoteAddr string
	Origin     string
	UserAgent  string
}

type ConnectionEventFn func(c *Connection, meta *ConnectionMetadata)

type Server struct {
	// *Connection -> *ConnectionMetadata
	connections sync.Map

	count atomic.Int64

	onNewConnection    ConnectionEventFn
	onClosedConnection ConnectionEventFn
}

func NewServer(onNewConnection, onClosedConnection ConnectionEventFn) *Server {
	return &Server{
		onNewConnection:    onNewConnection,
		onClosedConnection: onClosedConnection,
	}
}

func (s *Server) Handle(w http.ResponseWriter, r *http.Request) {
	upgrader := websocket.Upgrader{
		ReadBufferSize:  readBuffer,
		WriteBufferSize: writeBuffer,
		WriteBufferPool: bufferPool,
	}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		//log.Debug("WebSocket upgrade failed", "err", err)
		return
	}
	metadata := &ConnectionMetadata{
		RemoteAddr: r.RemoteAddr,
		Origin:     r.Header.Get("Origin"),
		UserAgent:  r.Header.Get("User-Agent"),
	}
	wc := setupConnection(conn)
	s.connections.Store(wc, metadata)
	defer s.connections.Delete(wc)
	s.count.Add(1)
	defer s.count.Add(-1)
	s.onNewConnection(wc, metadata)
	defer s.onClosedConnection(wc, metadata)
	closeCtx := wc.CloseCtx()
	// wait for connection to be closed
	<-closeCtx.Done()
}

func (s *Server) Range(fn func(wc *Connection, meta *ConnectionMetadata) bool) {
	s.connections.Range(func(key, value any) bool {
		return fn(key.(*Connection), value.(*ConnectionMetadata))
	})
}

func (s *Server) Count() int64 {
	return s.count.Load()
}
