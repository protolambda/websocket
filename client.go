package websocket

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"

	"github.com/gorilla/websocket"
)

func Dial(ctx context.Context, endpoint string) (*Connection, error) {
	dialer := &websocket.Dialer{
		Proxy:             http.ProxyFromEnvironment,
		ReadBufferSize:    readBuffer,
		WriteBufferSize:   writeBuffer,
		WriteBufferPool:   bufferPool,
		EnableCompression: true,
	}
	header := make(http.Header)
	conn, resp, err := dialer.DialContext(ctx, endpoint, header)
	if resp != nil {
		defer resp.Body.Close() // note: this becomes a No-op closer if successfully upgraded to websocket.
	}
	if err != nil {
		if resp != nil {
			err = fmt.Errorf("response status %s, err: %w", resp.Status, err)
		}
		return nil, fmt.Errorf("failed to dial websocket: %w", err)
	}
	return setupConnection(conn), nil
}

// Client lazily connects to the configured endpoint on writes/reads when necessary,
// until the reconnecting-client is Close-ed.
// The status of the current connection can be checked with Err().
type Client struct {
	reconnectCtx    context.Context // no reconnects will be attempted if ctx is closed
	reconnectCancel context.CancelFunc

	endpoint string

	connLock sync.Mutex
	conn     *Connection
}

func NewClient(endpoint string) *Client {
	ctx, cancel := context.WithCancel(context.Background())
	return &Client{
		reconnectCtx:    ctx,
		reconnectCancel: cancel,
		endpoint:        endpoint,
	}
}

func (rc *Client) Write(messageType MessageType, data []byte) error {
	conn, err := rc.reconnectMaybe()
	if err != nil {
		return err
	}
	return conn.Write(messageType, data)
}

func (rc *Client) Read() (messageType MessageType, p []byte, err error) {
	conn, err := rc.reconnectMaybe()
	if err != nil {
		return MessageType(0), nil, err
	}
	return conn.Read()
}

var ErrNotReconnecting = errors.New("not reconnecting")

func (rc *Client) reconnectMaybe() (*Connection, error) {
	rc.connLock.Lock()
	defer rc.connLock.Unlock()
	if rc.conn == nil || rc.conn.CloseCtx().Err() != nil {
		if rc.reconnectCtx.Err() != nil {
			return nil, ErrNotReconnecting
		}
		conn, err := Dial(rc.reconnectCtx, rc.endpoint)
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

var ErrNotConnected = errors.New("not connected")

// Err returns nil if the client is connected. It returns ErrNotConnected if not connected.
// It returns context.Canceled if the Reconnecting client was closed.
// It returns another error if the underlying connection failed or closed in some way.
// Client will attempt re-connection upon next Read or Write.
func (rc *Client) Err() error {
	rc.connLock.Lock()
	defer rc.connLock.Unlock()
	if rc.conn == nil {
		return ErrNotConnected
	}
	return context.Cause(rc.conn.CloseCtx())
}

func (rc *Client) Close() error {
	rc.reconnectCancel() // stop allowing reconnects
	// now close the underlying connection
	rc.connLock.Lock()
	defer rc.connLock.Unlock()
	return rc.conn.Close()
}
