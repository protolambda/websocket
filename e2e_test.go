package websocket

import (
	"context"
	"github.com/gorilla/websocket"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWebsocket(t *testing.T) {
	wsSrv := NewServer(func(c *Connection, meta *ConnectionMetadata) {
		t.Log("new connection", "origin:", meta.Origin,
			"remote:", meta.RemoteAddr, "user-agent:", meta.UserAgent)
	}, func(c *Connection, meta *ConnectionMetadata) {
		t.Log("closed connection", "origin:", meta.Origin,
			"remote:", meta.RemoteAddr, "user-agent:", meta.UserAgent,
			"err:", c.CloseCtx().Err(),
			"cause:", context.Cause(c.CloseCtx()))
	})
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", wsSrv.Handle)
	httpSrv := httptest.NewServer(mux)
	t.Cleanup(httpSrv.Close)
	rc := NewReconnectingClient(strings.ReplaceAll(httpSrv.URL, "http://", "ws://") + "/ws")
	err := rc.Write(TextMessage, []byte("hello world"))
	if err != nil {
		t.Fatal(err)
	}
	checkedServer := false
	wsSrv.Range(func(c *Connection, m *ConnectionMetadata) bool {
		checkedServer = true
		if m.RemoteAddr == "" {
			t.Fatal("expected metadata")
		}
		typ, msg, err := c.Read()
		if err != nil {
			t.Fatal("server failed to read", err)
		}
		if typ != TextMessage {
			t.Fatal("unexpected type", typ)
		}
		if string(msg) != "hello world" {
			t.Fatal("unexpected message", string(msg))
		}
		err = c.Write(TextMessage, []byte("server says hi"))
		if err != nil {
			t.Fatal("server failed to write", err)
		}
		if err := c.Close(); err != nil {
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
