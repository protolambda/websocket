package websocket

import (
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gorilla/websocket"
)

func TestServerClose(t *testing.T) {
	t.Parallel()
	var disconnects atomic.Int32
	srv := NewServer(func(c *Connection, meta *ConnectionMetadata) (*Connection, error) {
		go readLoop(c)
		return c, nil
	}, WithOnDisconnect(func(c *Connection) {
		if !errors.Is(c.Err(), ErrServerClosed) {
			t.Error("expected server closed cause", c.Err())
		}
		disconnects.Add(1)
	}))
	url := serve(t, srv)
	a := dial(t, url)
	b := dial(t, url)
	waitFor(t, func() bool { return srv.Count() == 2 })

	srv.Close()
	if n := disconnects.Load(); n != 2 {
		t.Fatal("expected disconnects before Close returns", n)
	}
	if n := srv.Count(); n != 0 {
		t.Fatal("expected no connections", n)
	}
	for _, conn := range []*Connection{a, b} {
		msg := await(t, readAsync(conn))
		if !websocket.IsCloseError(msg.err, websocket.CloseNormalClosure) {
			t.Fatal("expected close message", msg.err)
		}
	}
	_, err := Dial(t.Context(), url)
	if err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatal("expected unavailable server", err)
	}
}
