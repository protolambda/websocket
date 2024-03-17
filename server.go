package websocket

import (
	"net/http"
	"sync"

	"github.com/gorilla/websocket"
)

type ConnectionMetadata struct {
	RemoteAddr string
	Origin     string
	UserAgent  string
}

type Server struct {
	// *Connection -> *ConnectionMetadata
	connections sync.Map
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
	closeCtx := wc.CloseCtx()
	// wait for connection to be closed
	<-closeCtx.Done()
}
