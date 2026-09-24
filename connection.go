package websocket

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

const (
	readBuffer  = 1024
	writeBuffer = 1024
)

var bufferPool = new(sync.Pool)

// ErrPongTimeout is the cause of a connection closed because the peer did not answer a ping in time.
var ErrPongTimeout = errors.New("websocket peer did not answer ping in time")

func newConnection(conn *websocket.Conn, cfg connConfig) *Connection {
	closeCtx, closeCancel := context.WithCancelCause(context.Background())
	c := &Connection{
		conn:        conn,
		cfg:         cfg,
		created:     time.Now(),
		stop:        make(chan struct{}),
		ctxClose:    closeCtx,
		cancelClose: closeCancel,
	}
	conn.SetReadLimit(cfg.readLimit)
	// The handlers run within Read, and must not block it.
	conn.SetPongHandler(func(string) error {
		c.received()
		return nil
	})
	replyPong := conn.PingHandler()
	conn.SetPingHandler(func(appData string) error {
		c.received()
		return replyPong(appData)
	})
	if cfg.pingInterval > 0 {
		c.wg.Add(1)
		go c.keepAlive()
	}
	return c
}

// Connection is an opinionated wrapper around a Gorilla websocket connection.
// It handles pings, pongs, and close messages, and reports why the connection closed.
//
// Read and Write may be called concurrently with each other. Concurrent Read calls are serialized,
// as are concurrent Write calls. Close, CloseWithCause, Err, CloseCtx and the address methods
// may be called from any goroutine.
//
// Any error of Read or Write closes the connection, and becomes the cause of the closure (see Err).
// Read and Write on a closed connection return that cause.
//
// The connection owns a keepalive goroutine, which sends a ping when nothing was received from the peer
// for the ping interval (see WithPingInterval). The goroutine stops when the connection closes,
// and closes the connection if a ping cannot be written.
// Messages from the peer, including pongs and its close message, are only processed while Read is called.
// So a dead peer is detected while reading: Read fails with ErrPongTimeout if no message, ping or pong is received
// for the ping interval plus the pong timeout (see WithPongTimeout). A connection that is not read is not closed
// for that, but notices a dead peer only when it reads again, or when a write fails.
type Connection struct {
	conn *websocket.Conn
	cfg  connConfig

	// created is the reference time of lastRecv.
	created time.Time
	// lastRecv is the time since created at which the peer last sent something.
	lastRecv atomic.Int64

	// stop is closed when the connection starts closing, to stop the keepalive goroutine.
	stop chan struct{}
	// wg tracks the keepalive goroutine.
	wg sync.WaitGroup

	// to detect closing state externally, and to report the cause
	ctxClose    context.Context
	cancelClose context.CancelCauseFunc

	closer sync.Once

	// To avoid concurrent writing to the connection.
	// After acquiring the lock the write-timeout on the connection should be set.
	writeLock sync.Mutex

	// To avoid concurrent reading from the connection.
	// Only the holder of the lock may change the read deadline.
	readLock sync.Mutex
}

var _ Messenger = (*Connection)(nil)

// CloseCtx returns the context that terminates when the connection closed.
// The context Cause shares the reason for closure.
// This may simply be "context.Canceled" if Close() was called.
// This may be a websocket.CloseError if the peer closed the connection.
func (c *Connection) CloseCtx() context.Context {
	return c.ctxClose
}

// Err is a shorthand for the Cause error of the CloseCtx. It returns nil while the connection is open.
func (c *Connection) Err() error {
	return context.Cause(c.CloseCtx())
}

// LocalAddr returns the local network address of the connection.
func (c *Connection) LocalAddr() net.Addr {
	return c.conn.LocalAddr()
}

// RemoteAddr returns the network address of the peer.
func (c *Connection) RemoteAddr() net.Addr {
	return c.conn.RemoteAddr()
}

// Close closes the connection gracefully, if it's not already closed: see CloseWithCause.
// It then returns the error of the connection closing, or nil if successfully closed without issue.
func (c *Connection) Close() error {
	c.CloseWithCause(context.Canceled)
	err := c.Err()
	// intentional error equality. (wrapped context.Canceled errors are worth reporting)
	if err == context.Canceled {
		return nil
	}
	return err
}

// CloseWithCause closes the connection, if it's not already closed, and records cause as the reason (see Err).
// The first cause is kept. A nil cause is context.Canceled.
//
// If the cause is or wraps context.Canceled, the close is graceful: a close message is sent to the peer first,
// which takes up to the close timeout (see WithCloseTimeout). Any other cause is treated as a failure,
// and closes the connection without a close message.
//
// It blocks until the connection is closed, and the keepalive goroutine stopped.
func (c *Connection) CloseWithCause(cause error) {
	if cause == nil {
		cause = context.Canceled
	}
	c.closer.Do(func() {
		close(c.stop)
		// The keepalive goroutine may be writing a ping: it is awaited after closing the underlying connection,
		// which ends any write.
		if errors.Is(cause, context.Canceled) && c.cfg.closeTimeout > 0 {
			msg := websocket.FormatCloseMessage(websocket.CloseNormalClosure, "bye")
			err := c.conn.WriteControl(websocket.CloseMessage, msg, time.Now().Add(c.cfg.closeTimeout))
			// ErrCloseSent: the application already sent a close message.
			if err != nil && !errors.Is(err, websocket.ErrCloseSent) {
				cause = errors.Join(cause, fmt.Errorf("failed to write close message: %w", err))
			}
		}
		if err := c.conn.Close(); err != nil {
			cause = errors.Join(cause, fmt.Errorf("failed to close underlying connection: %w", err))
		}
		c.wg.Wait()
		c.cancelClose(cause)
	})
}

// Read reads the next message from the connection.
// Pings and pongs from the peer are handled while reading, and are not returned.
// If the peer closed the connection, the error is a websocket.CloseError (of the Gorilla library).
func (c *Connection) Read() (messageType MessageType, p []byte, err error) {
	c.readLock.Lock()
	defer c.readLock.Unlock()
	if c.ctxClose.Err() != nil {
		return 0, nil, c.Err()
	}
	// The peer has until the deadline to send something; it is extended by every message, ping or pong.
	c.extendReadDeadline()
	typ, p, err := c.conn.ReadMessage()
	if err != nil {
		if netErr, ok := errors.AsType[net.Error](err); ok && netErr.Timeout() {
			err = fmt.Errorf("%w: %w", ErrPongTimeout, err)
		}
		c.CloseWithCause(err)
		return 0, nil, c.Err()
	}
	c.received()
	return MessageType(typ), p, nil
}

// Write writes a message to the connection, within the write timeout (see WithWriteTimeout).
func (c *Connection) Write(messageType MessageType, data []byte) error {
	c.writeLock.Lock()
	defer c.writeLock.Unlock()
	if c.ctxClose.Err() != nil {
		return c.Err()
	}
	_ = c.conn.SetWriteDeadline(c.writeDeadline())
	if err := c.conn.WriteMessage(int(messageType), data); err != nil {
		c.CloseWithCause(err)
		return c.Err()
	}
	return nil
}

// writeDeadline returns the deadline of a write that starts now. The zero time means no deadline.
func (c *Connection) writeDeadline() time.Time {
	if c.cfg.writeTimeout <= 0 {
		return time.Time{}
	}
	return time.Now().Add(c.cfg.writeTimeout)
}

// received notes that the peer sent something. Only the reader may call it.
func (c *Connection) received() {
	c.lastRecv.Store(int64(time.Since(c.created)))
	c.extendReadDeadline()
}

// extendReadDeadline gives the peer the ping interval plus the pong timeout to send something.
// Only the reader may call it.
func (c *Connection) extendReadDeadline() {
	if c.cfg.pingInterval <= 0 || c.cfg.pongTimeout <= 0 {
		return
	}
	_ = c.conn.SetReadDeadline(time.Now().Add(c.cfg.pingInterval + c.cfg.pongTimeout))
}

// keepAlive runs the pings, and closes the connection if a ping fails.
func (c *Connection) keepAlive() {
	err := c.pingLoop()
	// Done before closing: CloseWithCause waits for this goroutine.
	c.wg.Done()
	if err != nil {
		c.CloseWithCause(err)
	}
}

// pingLoop sends a ping whenever nothing was received for the ping interval, until the connection closes.
func (c *Connection) pingLoop() error {
	timer := time.NewTimer(c.cfg.pingInterval)
	defer timer.Stop()
	for {
		select {
		case <-c.stop:
			return nil
		case <-timer.C:
		}
		idle := time.Since(c.created) - time.Duration(c.lastRecv.Load())
		if idle < c.cfg.pingInterval {
			timer.Reset(c.cfg.pingInterval - idle)
			continue
		}
		err := c.conn.WriteControl(websocket.PingMessage, nil, c.writeDeadline())
		if errors.Is(err, websocket.ErrCloseSent) {
			// The application sent a close message, and awaits the answer of the peer.
			return nil
		}
		if err != nil {
			return fmt.Errorf("failed to write ping: %w", err)
		}
		timer.Reset(c.cfg.pingInterval)
	}
}
