package websocket

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// TestUnsolicitedPong checks that a pong without a ping does not stall the reader.
func TestUnsolicitedPong(t *testing.T) {
	t.Parallel()
	srv := NewServer(func(c *Connection, meta *ConnectionMetadata) (struct{}, error) {
		if err := c.Write(PongMessage, nil); err != nil {
			return struct{}{}, err
		}
		return struct{}{}, c.Write(TextMessage, []byte("after pong"))
	})
	conn := dial(t, serve(t, srv))
	msg := await(t, readAsync(conn))
	if msg.err != nil {
		t.Fatal("failed to read", msg.err)
	}
	if string(msg.data) != "after pong" {
		t.Fatal("unexpected message", string(msg.data))
	}
}

// TestApplicationPing checks that the pong to a ping of the application does not stall the reader.
func TestApplicationPing(t *testing.T) {
	t.Parallel()
	url := serveGorilla(t, func(conn *websocket.Conn) {
		// Gorilla answers the ping while reading.
		if _, _, err := conn.ReadMessage(); err != nil {
			t.Error("failed to read", err)
			return
		}
		if err := conn.WriteMessage(websocket.TextMessage, []byte("after ping")); err != nil {
			t.Error("failed to write", err)
		}
		<-t.Context().Done()
	})
	conn := dial(t, url)
	if err := conn.Write(PingMessage, nil); err != nil {
		t.Fatal("failed to ping", err)
	}
	if err := conn.Write(TextMessage, []byte("hello")); err != nil {
		t.Fatal("failed to write", err)
	}
	msg := await(t, readAsync(conn))
	if msg.err != nil {
		t.Fatal("failed to read", msg.err)
	}
	if string(msg.data) != "after ping" {
		t.Fatal("unexpected message", string(msg.data))
	}
}

// TestKeepAlive checks that pings keep an idle connection open, beyond the pong timeout.
func TestKeepAlive(t *testing.T) {
	t.Parallel()
	const idle = 500 * time.Millisecond
	srv := NewServer(func(c *Connection, meta *ConnectionMetadata) (struct{}, error) {
		go readLoop(c)
		time.AfterFunc(idle, func() {
			_ = c.Write(TextMessage, []byte("still here"))
		})
		return struct{}{}, nil
	}, WithConnOpts[struct{}](WithPingInterval(10*time.Millisecond), WithPongTimeout(100*time.Millisecond)))
	url := serve(t, srv)
	// before the dial, since the server starts idling when it accepts
	start := time.Now()
	conn := dial(t, url, WithPingInterval(10*time.Millisecond), WithPongTimeout(100*time.Millisecond))
	msg := await(t, readAsync(conn))
	if msg.err != nil {
		t.Fatal("failed to read", msg.err)
	}
	if string(msg.data) != "still here" {
		t.Fatal("unexpected message", string(msg.data))
	}
	if elapsed := time.Since(start); elapsed < idle {
		t.Fatal("read returned early", elapsed)
	}
}

// TestPongTimeout checks that a Read fails if the peer does not answer pings.
func TestPongTimeout(t *testing.T) {
	t.Parallel()
	url := serveGorilla(t, func(conn *websocket.Conn) {
		// Gorilla answers pings only while reading.
		<-t.Context().Done()
	})
	conn := dial(t, url, WithPingInterval(10*time.Millisecond), WithPongTimeout(50*time.Millisecond))
	msg := await(t, readAsync(conn))
	if !errors.Is(msg.err, ErrPongTimeout) {
		t.Fatal("expected pong timeout", msg.err)
	}
	if !errors.Is(conn.Err(), ErrPongTimeout) {
		t.Fatal("expected pong timeout as cause", conn.Err())
	}
}

// TestPingFailureCloses checks that the keepalive goroutine closes the connection when a ping fails,
// without waiting on itself.
func TestPingFailureCloses(t *testing.T) {
	t.Parallel()
	url := serveGorilla(t, func(conn *websocket.Conn) {})
	conn := dial(t, url, WithPingInterval(10*time.Millisecond))
	await(t, conn.CloseCtx().Done())
	if conn.Err() == nil {
		t.Fatal("expected a cause")
	}
	t.Log("cause:", conn.Err())
}

// TestCloseStalledWrite checks that Close returns while a write and a ping are stalled on a peer that does not read,
// also without write timeout.
func TestCloseStalledWrite(t *testing.T) {
	t.Parallel()
	url := serveGorilla(t, func(conn *websocket.Conn) {
		<-t.Context().Done()
	})
	conn := dial(t, url, WithWriteTimeout(0), WithPingInterval(10*time.Millisecond), WithCloseTimeout(50*time.Millisecond))
	var writes atomic.Int64
	go func() {
		msg := make([]byte, 1<<20)
		for conn.Write(BinaryMessage, msg) == nil {
			writes.Add(1)
		}
	}()
	// Stalled once the socket buffers are full.
	for last := int64(-1); last != writes.Load(); {
		last = writes.Load()
		time.Sleep(100 * time.Millisecond)
	}
	closed := make(chan error, 1)
	go func() {
		closed <- conn.Close()
	}()
	if err := await(t, closed); err == nil {
		t.Fatal("expected close message to fail")
	}
}

// TestReadErrorCloses checks that a read error other than a close message closes the connection.
func TestReadErrorCloses(t *testing.T) {
	t.Parallel()
	disconnected := make(chan error, 1)
	srv := NewServer(func(c *Connection, meta *ConnectionMetadata) (*Connection, error) {
		go readLoop(c)
		return c, nil
	}, WithOnDisconnect(func(c *Connection) {
		disconnected <- c.Err()
	}), WithConnOpts[*Connection](WithReadLimit(16)))
	conn := dial(t, serve(t, srv))
	if err := conn.Write(TextMessage, make([]byte, 64)); err != nil {
		t.Fatal("failed to write", err)
	}
	if err := await(t, disconnected); !errors.Is(err, websocket.ErrReadLimit) {
		t.Fatal("expected read limit error", err)
	}
	if n := srv.Count(); n != 0 {
		t.Fatal("expected no connections", n)
	}
	msg := await(t, readAsync(conn))
	if !websocket.IsCloseError(msg.err, websocket.CloseMessageTooBig) {
		t.Fatal("expected close message from server", msg.err)
	}
}

// TestWriteErrorCloses checks that a write error closes the connection.
func TestWriteErrorCloses(t *testing.T) {
	t.Parallel()
	srv := NewServer(func(c *Connection, meta *ConnectionMetadata) (struct{}, error) {
		go readLoop(c)
		return struct{}{}, nil
	})
	conn := dial(t, serve(t, srv))
	if err := conn.Write(MessageType(3), nil); err == nil {
		t.Fatal("expected error for reserved message type")
	}
	if conn.Err() == nil {
		t.Fatal("expected closed connection")
	}
}

// TestUseAfterClose checks that Read and Write on a closed connection return the cause.
func TestUseAfterClose(t *testing.T) {
	t.Parallel()
	srv := NewServer(func(c *Connection, meta *ConnectionMetadata) (struct{}, error) {
		go readLoop(c)
		return struct{}{}, nil
	})
	conn := dial(t, serve(t, srv))
	if err := conn.Close(); err != nil {
		t.Fatal("failed to close", err)
	}
	// Gorilla panics after 1000 reads of a failed connection.
	for range 2000 {
		if _, _, err := conn.Read(); !errors.Is(err, context.Canceled) {
			t.Fatal("expected cause", err)
		}
	}
	if err := conn.Write(TextMessage, nil); !errors.Is(err, context.Canceled) {
		t.Fatal("expected cause", err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal("expected repeated close to succeed", err)
	}
}

// TestCloseWithoutMessage checks that a zero close timeout closes without a close message.
func TestCloseWithoutMessage(t *testing.T) {
	t.Parallel()
	srv := NewServer(func(c *Connection, meta *ConnectionMetadata) (struct{}, error) {
		return struct{}{}, c.Close()
	}, WithConnOpts[struct{}](WithCloseTimeout(0)))
	conn := dial(t, serve(t, srv))
	msg := await(t, readAsync(conn))
	if !websocket.IsCloseError(msg.err, websocket.CloseAbnormalClosure) {
		t.Fatal("expected abnormal closure", msg.err)
	}
}

// TestAddrs checks the addresses of a connection against the server's view.
func TestAddrs(t *testing.T) {
	t.Parallel()
	remote := make(chan string, 1)
	srv := NewServer(func(c *Connection, meta *ConnectionMetadata) (struct{}, error) {
		remote <- meta.RemoteAddr
		if got := c.RemoteAddr().String(); got != meta.RemoteAddr {
			t.Error("unexpected remote address", got, meta.RemoteAddr)
		}
		return struct{}{}, nil
	})
	url := serve(t, srv)
	conn := dial(t, url)
	if got, want := conn.LocalAddr().String(), await(t, remote); got != want {
		t.Fatal("unexpected local address", got, want)
	}
	if got := conn.RemoteAddr().String(); "ws://"+got != url {
		t.Fatal("unexpected remote address", got, url)
	}
}
