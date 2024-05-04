package websocket

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	pingInterval        = 30 * time.Second
	pingWriteTimeout    = 5 * time.Second
	pongTimeout         = 30 * time.Second
	defaultWriteTimeout = 10 * time.Second
)

const (
	readBuffer  = 1024
	writeBuffer = 1024
)

var bufferPool = new(sync.Pool)

func setupConnection(conn *websocket.Conn) *Connection {
	wsCtx, wsCancel := context.WithCancel(context.Background())
	closeCtx, closeCancel := context.WithCancelCause(context.Background())
	wc := &Connection{
		conn:         conn,
		pongReceived: make(chan struct{}),
		pingReset:    make(chan struct{}, 1),
		ctxRes:       wsCtx,
		cancelRes:    wsCancel,
		ctxClose:     closeCtx,
		cancelClose:  closeCancel,
	}
	conn.SetPongHandler(func(appData string) error {
		select {
		case wc.pongReceived <- struct{}{}:
		case <-wc.ctxRes.Done():
		}
		return nil
	})
	return wc
}

// Connection is an opinionated wrapper around the Gorilla websocket connection library.
// It handles pings/pongs/reads/writes/close.
// This manages the closing of a connection by reporting *why* the connection was closed,
// and sending a close-message to the wbesocket if we are closing.
type Connection struct {
	// to detect closing state internally, and clean up resources
	ctxRes    context.Context
	cancelRes context.CancelFunc

	// to detect closing state externally
	ctxClose    context.Context
	cancelClose context.CancelCauseFunc

	closer sync.Once

	// to wait for all sub-resources to close
	wg sync.WaitGroup

	conn *websocket.Conn

	// To avoid concurrent writing to the connection.
	// After acquiring the lock the write-timeout on the connection should be set.
	writeLock sync.Mutex

	// to avoid
	readLock sync.Mutex

	// to reset read deadline upon detecting liveness
	pongReceived chan struct{}
	// to reset time till next ping, upon writing a regular message
	pingReset chan struct{}
}

var _ Messenger = (*Connection)(nil)

// CloseCtx returns the context that terminates when the connection closed.
// The context Cause shares the reason for closure.
// This may simply be "context.Canceled" if Close() was called.
// This may be a websocket.CloseError if the connection itself was broken.
func (wc *Connection) CloseCtx() context.Context {
	return wc.ctxClose
}

// Err is a shorthand for the Cause error of the CloseCtx.
func (wc *Connection) Err() error {
	return context.Cause(wc.CloseCtx())
}

// Close closes the connection, if it's not already closed.
// It then returns the error of the connection closing, or nil if successfully closed without issue.
func (wc *Connection) Close() error {
	wc.CloseWithCause(context.Canceled)
	err := context.Cause(wc.ctxClose)
	// intentional error equality. (wrapped context.Canceled errors are worth reporting)
	if err == context.Canceled {
		err = nil
	}
	return err
}

func (wc *Connection) CloseWithCause(cause error) {
	wc.closer.Do(func() {
		wc.cancelRes()
		wc.wg.Wait()                            // wait for all usages to complete
		if errors.Is(cause, context.Canceled) { // if we are just choosing to close ourselves, send a nice close message.
			// no-need to lock, this is the last remaining usage of the connection
			err := wc.conn.WriteControl(websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.CloseNormalClosure, "bye"),
				time.Now().Add(pingWriteTimeout))
			if err != nil {
				cause = errors.Join(cause, fmt.Errorf("failed to write close message: %w", err))
			}
		}
		if err := wc.conn.Close(); err != nil {
			cause = errors.Join(cause, fmt.Errorf("failed to close underlying connection: %w", err))
		}
		wc.cancelClose(cause)
	})
}

func (wc *Connection) readHelper() (messageType MessageType, p []byte, err error) {
	wc.readLock.Lock()
	defer wc.readLock.Unlock()
	var typ int
	typ, p, err = wc.conn.ReadMessage()
	messageType = MessageType(typ)
	return
}

// Read reads from the connection.
// Note: reads on the underlying connection are timed out by the underlying ping-pong message system.
func (wc *Connection) Read() (messageType MessageType, p []byte, err error) {
	messageType, p, err = wc.readHelper()
	if websocket.IsUnexpectedCloseError(err) {
		wc.CloseWithCause(err)
	}
	return
}

func (wc *Connection) writeHelper(messageType MessageType, data []byte) error {
	wc.writeLock.Lock()
	defer wc.writeLock.Unlock()
	deadline := time.Now().Add(defaultWriteTimeout)
	_ = wc.conn.SetWriteDeadline(deadline)
	return wc.conn.WriteMessage(int(messageType), data)
}

// Write writes to the connection.
func (wc *Connection) Write(messageType MessageType, data []byte) error {
	err := wc.writeHelper(messageType, data)
	if err == nil {
		select {
		case wc.pingReset <- struct{}{}:
		default:
		}
	}
	if websocket.IsUnexpectedCloseError(err) {
		wc.CloseWithCause(err)
	}
	return err
}

func (wc *Connection) pingPong() {
	var pingTimer = time.NewTimer(pingInterval)
	defer wc.wg.Done()
	defer pingTimer.Stop()

	for {
		select {
		case <-wc.ctxRes.Done():
			return

		case <-wc.pingReset:
			if !pingTimer.Stop() {
				<-pingTimer.C
			}
			pingTimer.Reset(pingInterval)

		case <-pingTimer.C:
			wc.writeLock.Lock()
			_ = wc.conn.SetWriteDeadline(time.Now().Add(pingWriteTimeout))
			err := wc.conn.WriteMessage(websocket.PingMessage, nil)
			_ = wc.conn.SetReadDeadline(time.Now().Add(pongTimeout))
			wc.writeLock.Unlock()
			if websocket.IsUnexpectedCloseError(err) {
				wc.CloseWithCause(err)
			}
			pingTimer.Reset(pingInterval)

		case <-wc.pongReceived:
			_ = wc.conn.SetReadDeadline(time.Time{})
		}
	}
}
