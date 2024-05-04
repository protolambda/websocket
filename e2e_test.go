package websocket

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
)

type basicConnData struct {
	c    *Connection
	meta *ConnectionMetadata
}

func TestWebsocket(t *testing.T) {
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
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", wsSrv.Handle)
	httpSrv := httptest.NewServer(mux)
	t.Cleanup(httpSrv.Close)
	rc := NewClient(strings.ReplaceAll(httpSrv.URL, "http://", "ws://") + "/ws")
	err := rc.Write(TextMessage, []byte("hello world"))
	if err != nil {
		t.Fatal(err)
	}
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
