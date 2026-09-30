package websocket

import (
	"bytes"
	"compress/flate"
	"context"
	"errors"
	"fmt"
	"math"
	"net"
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
		// Small socket buffers, on both sides, so that the writes stall after a few KiB on any machine. With the
		// buffers that the kernel sizes itself (several MiB), a slow machine (e.g. with the race detector, which
		// makes masking the client's frames slow) keeps writing through the whole test, and the close message goes
		// out between two frames.
		setBuffer(t, conn.NetConn(), (*net.TCPConn).SetReadBuffer)
		<-t.Context().Done()
	})
	conn := dial(t, url, WithWriteTimeout(0), WithPingInterval(10*time.Millisecond), WithCloseTimeout(50*time.Millisecond))
	setBuffer(t, conn.conn.NetConn(), (*net.TCPConn).SetWriteBuffer)
	var writes atomic.Int64
	go func() {
		msg := make([]byte, 1<<20)
		for conn.Write(BinaryMessage, msg) == nil {
			writes.Add(1)
		}
	}()
	// Stalled once the socket buffers are full. A pause of the writes alone does not prove it, so a ping must not get
	// through within 200ms either: the stalled writer holds the connection's write lock without a deadline.
	for stalled := false; !stalled; {
		for last := int64(-1); last != writes.Load(); {
			last = writes.Load()
			time.Sleep(100 * time.Millisecond)
		}
		stalled = conn.conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(200*time.Millisecond)) != nil
		if t.Context().Err() != nil {
			t.Fatal("the writes did not stall")
		}
	}
	closed := make(chan error, 1)
	go func() {
		closed <- conn.Close()
	}()
	if err := await(t, closed); err == nil {
		t.Fatal("expected close message to fail")
	}
}

// setBuffer sets a socket buffer of the TCP connection c to 4 KiB with set.
func setBuffer(t *testing.T, c net.Conn, set func(*net.TCPConn, int) error) {
	t.Helper()
	tcp, ok := c.(*net.TCPConn)
	if !ok {
		t.Fatalf("not a TCP connection: %T", c)
	}
	if err := set(tcp, 4<<10); err != nil {
		t.Fatal("failed to set the socket buffer:", err)
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

// forwardReads reads from c until it fails, and sends the result of every Read to out.
func forwardReads(c *Connection, out chan<- readMsg) {
	for {
		typ, data, err := c.Read()
		out <- readMsg{typ: typ, data: data, err: err}
		if err != nil {
			return
		}
	}
}

// TestReadLimitDecompressed checks that the read limit bounds a compressed message after decompression,
// on the server and on the client (which enables compression by default):
// the messages cross the network in fewer bytes than the limit, but inflate beyond it.
func TestReadLimitDecompressed(t *testing.T) {
	t.Parallel()
	const limit = 4096
	// 64 times the limit, and compresses to about 300 bytes: one frame, since the write buffer holds 1 KiB.
	// The reader closes the connection once the limit is exceeded; a sender still writing a later frame of the
	// message would fail with a connection reset.
	big := bytes.Repeat([]byte{'a'}, 256<<10)
	if n := deflatedSize(t, big); n >= writeBuffer {
		t.Fatal("expected the message to compress into one frame", n)
	}
	checkRead := func(t *testing.T, msg readMsg, size int) {
		t.Helper()
		if msg.err != nil || len(msg.data) != size {
			t.Fatal("unexpected read", len(msg.data), msg.err)
		}
	}
	checkLimit := func(t *testing.T, msg readMsg) {
		t.Helper()
		if !errors.Is(msg.err, websocket.ErrReadLimit) {
			t.Fatal("expected read limit error", len(msg.data), msg.err)
		}
	}
	checkWire := func(t *testing.T, n int64) {
		t.Helper()
		// Else the frames, not the decompressed message, were limited.
		if n >= limit {
			t.Fatal("expected compressed messages within the limit on the network", n)
		}
		t.Log("bytes on the network, with the handshake:", n)
	}
	for _, size := range []int{limit + 1, len(big)} {
		t.Run(fmt.Sprintf("server/%d", size), func(t *testing.T) {
			t.Parallel()
			serverReads := make(chan readMsg, 16)
			srv := NewServer(func(c *Connection, meta *ConnectionMetadata) (struct{}, error) {
				go forwardReads(c, serverReads)
				return struct{}{}, nil
			}, WithConnOpts[struct{}](WithCompression(true), WithReadLimit(limit)))
			url, wire := serveCounted(t, srv)
			conn := dial(t, url)
			if err := conn.Write(BinaryMessage, big[:limit]); err != nil {
				t.Fatal("failed to write", err)
			}
			if err := conn.Write(BinaryMessage, big[:size]); err != nil {
				t.Fatal("failed to write", err)
			}
			checkRead(t, await(t, serverReads), limit)
			checkLimit(t, await(t, serverReads))
			checkWire(t, wire.read.Load())
			msg := await(t, readAsync(conn))
			if !websocket.IsCloseError(msg.err, websocket.CloseMessageTooBig) {
				t.Fatal("expected close message from server", msg.err)
			}
		})
		t.Run(fmt.Sprintf("client/%d", size), func(t *testing.T) {
			t.Parallel()
			serverReads := make(chan readMsg, 16)
			srv := NewServer(func(c *Connection, meta *ConnectionMetadata) (struct{}, error) {
				go forwardReads(c, serverReads)
				if err := c.Write(BinaryMessage, big[:limit]); err != nil {
					return struct{}{}, err
				}
				return struct{}{}, c.Write(BinaryMessage, big[:size])
			}, WithConnOpts[struct{}](WithCompression(true)))
			url, wire := serveCounted(t, srv)
			conn := dial(t, url, WithReadLimit(limit))
			checkRead(t, await(t, readAsync(conn)), limit)
			checkLimit(t, await(t, readAsync(conn)))
			if !errors.Is(conn.Err(), websocket.ErrReadLimit) {
				t.Fatal("expected read limit as cause", conn.Err())
			}
			msg := await(t, serverReads)
			if !websocket.IsCloseError(msg.err, websocket.CloseMessageTooBig) {
				t.Fatal("expected close message from client", msg.err)
			}
			checkWire(t, wire.written.Load())
		})
	}
}

// deflatedSize returns the size of p compressed like the Gorilla library does: deflate at level 1, flushed,
// without the 4 bytes of the final empty block.
func deflatedSize(t *testing.T, p []byte) int {
	t.Helper()
	var buf bytes.Buffer
	w, err := flate.NewWriter(&buf, flate.BestSpeed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(p); err != nil {
		t.Fatal(err)
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	return buf.Len() - 4
}

// TestReadLimitWithoutCloseTimeout checks the close message for a message beyond the read limit,
// with a zero close timeout: it is skipped for a decompressed message, while the Gorilla library sends it
// for frames beyond the limit.
func TestReadLimitWithoutCloseTimeout(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		compression bool
		code        int
	}{
		{compression: true, code: websocket.CloseAbnormalClosure},
		{compression: false, code: websocket.CloseMessageTooBig},
	} {
		t.Run(fmt.Sprintf("compression=%v", tc.compression), func(t *testing.T) {
			t.Parallel()
			disconnected := make(chan error, 1)
			srv := NewServer(func(c *Connection, meta *ConnectionMetadata) (*Connection, error) {
				go readLoop(c)
				return c, nil
			}, WithOnDisconnect(func(c *Connection) {
				disconnected <- c.Err()
			}), WithConnOpts[*Connection](WithCompression(tc.compression), WithReadLimit(64), WithCloseTimeout(0)))
			conn := dial(t, serve(t, srv), WithCompression(tc.compression))
			// Compresses to fewer than 64 bytes.
			if err := conn.Write(BinaryMessage, make([]byte, 1024)); err != nil {
				t.Fatal("failed to write", err)
			}
			if err := await(t, disconnected); !errors.Is(err, websocket.ErrReadLimit) {
				t.Fatal("expected read limit error", err)
			}
			msg := await(t, readAsync(conn))
			if !websocket.IsCloseError(msg.err, tc.code) {
				t.Fatal("unexpected close", tc.code, msg.err)
			}
		})
	}
}

// TestNoReadLimitCompressed checks that a compressed message is not limited without a read limit,
// or with the largest one.
func TestNoReadLimitCompressed(t *testing.T) {
	t.Parallel()
	big := bytes.Repeat([]byte{'a'}, 1<<20)
	for _, limit := range []int64{0, math.MaxInt64} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			t.Parallel()
			serverReads := make(chan readMsg, 16)
			srv := NewServer(func(c *Connection, meta *ConnectionMetadata) (struct{}, error) {
				go forwardReads(c, serverReads)
				return struct{}{}, c.Write(BinaryMessage, big)
			}, WithConnOpts[struct{}](WithCompression(true), WithReadLimit(limit)))
			conn := dial(t, serve(t, srv), WithReadLimit(limit))
			if err := conn.Write(BinaryMessage, big); err != nil {
				t.Fatal("failed to write", err)
			}
			for _, msg := range []readMsg{await(t, readAsync(conn)), await(t, serverReads)} {
				if msg.err != nil || !bytes.Equal(msg.data, big) {
					t.Fatal("unexpected read", len(msg.data), msg.err)
				}
			}
		})
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

// TestWriteWhileClosing checks that a Write that fails because Read sent a close message does not replace the cause
// of the closure. Read sends a close message and then closes the connection: with status 1009 for a message beyond
// the read limit, and to answer the close message of the peer.
func TestWriteWhileClosing(t *testing.T) {
	t.Parallel()
	srv := NewServer(func(c *Connection, meta *ConnectionMetadata) (struct{}, error) {
		go readLoop(c)
		return struct{}{}, nil
	})
	conn := dial(t, serve(t, srv))
	// What Read does before it closes the connection with the cause.
	msg := websocket.FormatCloseMessage(websocket.CloseMessageTooBig, "")
	if err := conn.conn.WriteControl(websocket.CloseMessage, msg, time.Time{}); err != nil {
		t.Fatal("failed to write close message", err)
	}
	written := make(chan error, 1)
	go func() {
		written <- conn.Write(TextMessage, []byte("racing"))
	}()
	select {
	case err := <-written:
		t.Fatal("expected Write to wait for the closure", err, conn.Err())
	case <-time.After(50 * time.Millisecond):
	}
	conn.CloseWithCause(websocket.ErrReadLimit)
	if err := await(t, written); !errors.Is(err, websocket.ErrReadLimit) {
		t.Fatal("expected the cause of the closure", err)
	}
	if err := conn.Err(); !errors.Is(err, websocket.ErrReadLimit) {
		t.Fatal("expected read limit as cause", err)
	}
}

// TestWriteAfterCloseMessage checks that a Write after the close message of the application fails,
// without waiting for the peer to answer the close message.
func TestWriteAfterCloseMessage(t *testing.T) {
	t.Parallel()
	srv := NewServer(func(c *Connection, meta *ConnectionMetadata) (struct{}, error) {
		go readLoop(c)
		return struct{}{}, nil
	})
	conn := dial(t, serve(t, srv))
	if err := conn.Write(CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "")); err != nil {
		t.Fatal("failed to write close message", err)
	}
	written := make(chan error, 1)
	go func() {
		written <- conn.Write(TextMessage, []byte("after close"))
	}()
	if err := await(t, written); !errors.Is(err, websocket.ErrCloseSent) {
		t.Fatal("expected close sent error", err)
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
