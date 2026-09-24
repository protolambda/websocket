package websocket

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"

	"github.com/gorilla/websocket"
)

// Dial opens a websocket connection to the endpoint, a ws:// or wss:// URL.
// The context bounds the dial, including the handshake: canceling it aborts the dial.
// It does not affect the returned connection.
// It connects directly, unless WithProxy is given.
func Dial(ctx context.Context, endpoint string, opts ...DialOpt) (*Connection, error) {
	cfg := newDialConfig(opts)
	var abort dialAborter
	var netDialer net.Dialer
	dialer := &websocket.Dialer{
		NetDialContext: func(dialCtx context.Context, network, addr string) (net.Conn, error) {
			conn, err := netDialer.DialContext(dialCtx, network, addr)
			if err != nil {
				return nil, err
			}
			abort.watch(ctx, conn)
			return conn, nil
		},
		Proxy:             cfg.proxy,
		HandshakeTimeout:  cfg.handshakeTimeout,
		ReadBufferSize:    readBuffer,
		WriteBufferSize:   writeBuffer,
		WriteBufferPool:   bufferPool,
		EnableCompression: cfg.conn.compression,
	}
	// The response body does not need to be closed: the dialer replaces it.
	conn, resp, err := dialer.DialContext(ctx, endpoint, cfg.header)
	aborted := !abort.release()
	if err != nil {
		if resp != nil {
			err = fmt.Errorf("response status %s: %w", resp.Status, err)
		}
		if cause := context.Cause(ctx); cause != nil {
			err = fmt.Errorf("%w: %w", cause, err)
		}
		return nil, fmt.Errorf("failed to dial websocket: %w", err)
	}
	if aborted {
		_ = conn.Close()
		return nil, fmt.Errorf("failed to dial websocket: %w", context.Cause(ctx))
	}
	return newConnection(conn, cfg.conn), nil
}

// dialAborter closes the network connections of a dial when its context is done.
// The Gorilla dialer only applies the deadline of the context to the handshake, not its cancellation.
type dialAborter struct {
	mu    sync.Mutex
	stops []func() bool
}

func (a *dialAborter) watch(ctx context.Context, conn net.Conn) {
	stop := context.AfterFunc(ctx, func() {
		_ = conn.Close()
	})
	a.mu.Lock()
	defer a.mu.Unlock()
	a.stops = append(a.stops, stop)
}

// release stops watching the connections.
// It returns false if the context was done first, and a connection may have been closed.
func (a *dialAborter) release() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	ok := true
	for _, stop := range a.stops {
		if !stop() {
			ok = false
		}
	}
	a.stops = nil
	return ok
}

// Client lazily connects to the configured endpoint on writes/reads when necessary,
// until the reconnecting-client is Close-ed.
// The status of the current connection can be checked with Err().
type Client struct {
	reconnectCtx    context.Context // no reconnects will be attempted if ctx is closed
	reconnectCancel context.CancelFunc

	endpoint string
	opts     []DialOpt

	connLock sync.Mutex
	conn     *Connection
}

var _ Messenger = (*Client)(nil)

// NewClient creates a Client for the endpoint. It dials with the given options (see Dial) when it connects.
func NewClient(endpoint string, opts ...DialOpt) *Client {
	ctx, cancel := context.WithCancel(context.Background())
	return &Client{
		reconnectCtx:    ctx,
		reconnectCancel: cancel,
		endpoint:        endpoint,
		opts:            opts,
	}
}

// Write writes a message, and connects first if there is no open connection.
func (rc *Client) Write(messageType MessageType, data []byte) error {
	conn, err := rc.reconnectMaybe()
	if err != nil {
		return err
	}
	return conn.Write(messageType, data)
}

// Read reads a message, and connects first if there is no open connection.
func (rc *Client) Read() (messageType MessageType, p []byte, err error) {
	conn, err := rc.reconnectMaybe()
	if err != nil {
		return MessageType(0), nil, err
	}
	return conn.Read()
}

// ErrNotReconnecting is returned by Read and Write after the Client is closed.
var ErrNotReconnecting = errors.New("not reconnecting")

func (rc *Client) reconnectMaybe() (*Connection, error) {
	rc.connLock.Lock()
	defer rc.connLock.Unlock()
	if rc.conn == nil || rc.conn.CloseCtx().Err() != nil {
		if rc.reconnectCtx.Err() != nil {
			return nil, ErrNotReconnecting
		}
		conn, err := Dial(rc.reconnectCtx, rc.endpoint, rc.opts...)
		if err != nil {
			if recErr := rc.reconnectCtx.Err(); recErr != nil && errors.Is(err, recErr) {
				return nil, ErrNotReconnecting
			}
			return nil, fmt.Errorf("dial failed: %w", err)
		}
		rc.conn = conn
	}
	// we return the connection, so reads/writes can be parallel
	return rc.conn, nil
}

// ErrNotConnected is returned by Client.Err if the client has not connected yet.
var ErrNotConnected = errors.New("not connected")

// Err returns nil if the client is connected. It returns ErrNotConnected if it has not connected yet.
// Otherwise it returns the cause of the closure of the last connection (see Connection.Err):
// context.Canceled if the Client was closed, or another error if the connection failed or closed in some way.
// Client will attempt re-connection upon next Read or Write, unless it was closed.
func (rc *Client) Err() error {
	rc.connLock.Lock()
	defer rc.connLock.Unlock()
	if rc.conn == nil {
		return ErrNotConnected
	}
	return context.Cause(rc.conn.CloseCtx())
}

// Close stops reconnecting, aborts a dial in progress, and closes the current connection (see Connection.Close).
// It returns nil if the client never connected.
func (rc *Client) Close() error {
	rc.reconnectCancel() // stop allowing reconnects
	// now close the underlying connection
	rc.connLock.Lock()
	defer rc.connLock.Unlock()
	if rc.conn == nil {
		return nil
	}
	return rc.conn.Close()
}
