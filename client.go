package websocket

import (
	"context"
	"fmt"
	"net/http"

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
