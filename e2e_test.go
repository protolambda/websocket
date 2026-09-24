package websocket

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// serve serves srv with a test HTTP server, and returns its websocket URL.
func serve[E any](t *testing.T, srv *Server[E]) string {
	t.Helper()
	httpSrv := httptest.NewServer(http.HandlerFunc(srv.Handle))
	t.Cleanup(func() {
		srv.Close()
		httpSrv.Close()
	})
	return "ws" + strings.TrimPrefix(httpSrv.URL, "http")
}

// serveGorilla serves plain Gorilla connections, and returns the websocket URL.
// The connection is closed when fn returns.
func serveGorilla(t *testing.T, fn func(conn *websocket.Conn)) string {
	t.Helper()
	httpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var upgrader websocket.Upgrader
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Error("failed to upgrade", err)
			return
		}
		defer conn.Close()
		fn(conn)
	}))
	t.Cleanup(httpSrv.Close)
	return "ws" + strings.TrimPrefix(httpSrv.URL, "http")
}

// dial dials the URL, and closes the connection when the test ends.
func dial(t *testing.T, url string, opts ...DialOpt) *Connection {
	t.Helper()
	conn, err := Dial(t.Context(), url, opts...)
	if err != nil {
		t.Fatal("failed to dial", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// readMsg is the result of a Read.
type readMsg struct {
	typ  MessageType
	data []byte
	err  error
}

// readAsync reads the next message in the background.
func readAsync(m Messenger) <-chan readMsg {
	out := make(chan readMsg, 1)
	go func() {
		typ, data, err := m.Read()
		out <- readMsg{typ: typ, data: data, err: err}
	}()
	return out
}

// await returns the next value of ch, or fails the test after 5 seconds.
func await[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(5 * time.Second):
		t.Fatal("timed out")
		var zero T
		return zero
	}
}

// waitFor polls cond until it holds, or fails the test after 5 seconds.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(time.Millisecond)
	}
}

// readLoop reads from c until it fails. A server reads to answer the pings of its peer.
func readLoop(c *Connection) {
	for {
		if _, _, err := c.Read(); err != nil {
			return
		}
	}
}

type basicConnData struct {
	c    *Connection
	meta *ConnectionMetadata
}

func TestWebsocket(t *testing.T) {
	t.Parallel()
	wsSrv := NewServer[*basicConnData](func(c *Connection, meta *ConnectionMetadata) (*basicConnData, error) {
		t.Log("new connection", "origin:", meta.Origin,
			"remote:", meta.RemoteAddr, "user-agent:", meta.UserAgent)
		return &basicConnData{c: c, meta: meta}, nil
	}, WithOnDisconnect(func(b *basicConnData) {
		t.Log("closed connection", "origin:", b.meta.Origin,
			"remote:", b.meta.RemoteAddr, "user-agent:", b.meta.UserAgent,
			"err:", b.c.CloseCtx().Err(),
			"cause:", context.Cause(b.c.CloseCtx()))
	}))
	rc := NewClient(serve(t, wsSrv))
	t.Cleanup(func() { _ = rc.Close() })
	err := rc.Write(TextMessage, []byte("hello world"))
	if err != nil {
		t.Fatal(err)
	}
	// The server registers the connection after the handshake.
	waitFor(t, func() bool { return wsSrv.Count() == 1 })
	checkedServer := false
	wsSrv.Range(func(b *basicConnData) bool {
		checkedServer = true
		if b.meta.RemoteAddr == "" {
			t.Fatal("expected metadata")
		}
		typ, msg, err := b.c.Read()
		if err != nil {
			t.Fatal("server failed to read", err)
		}
		if typ != TextMessage {
			t.Fatal("unexpected type", typ)
		}
		if string(msg) != "hello world" {
			t.Fatal("unexpected message", string(msg))
		}
		err = b.c.Write(TextMessage, []byte("server says hi"))
		if err != nil {
			t.Fatal("server failed to write", err)
		}
		if err := b.c.Close(); err != nil {
			t.Fatal("server failed to close connection")
		}
		return false
	})
	if !checkedServer {
		t.Fatal("didn't check server")
	}
	typ, msg, err := rc.Read()
	if err != nil {
		t.Fatal("server failed to read", err)
	}
	if typ != TextMessage {
		t.Fatal("unexpected type", typ)
	}
	if string(msg) != "server says hi" {
		t.Fatal("unexpected message", string(msg))
	}
	_, _, _ = rc.Read()
	cause := rc.Err()
	closeErr, ok := cause.(*websocket.CloseError)
	if !ok {
		t.Fatal("not a close error", cause)
	}
	if closeErr.Code != websocket.CloseNormalClosure {
		t.Fatal("not a normal close")
	}
	if closeErr.Text != "bye" {
		t.Fatal("expected bye message")
	}
}
