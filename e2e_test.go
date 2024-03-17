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
	wsSrv := Server{}
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", wsSrv.Handle)
	httpSrv := httptest.NewServer(mux)
	t.Cleanup(httpSrv.Close)
	conn, err := Dial(context.Background(), strings.ReplaceAll(httpSrv.URL, "http://", "ws://")+"/ws")
	if err != nil {
		t.Fatal(err)
	}
	err = conn.Write(websocket.TextMessage, []byte("hello world"))
	if err != nil {
		t.Fatal(err)
	}
	checkedServer := false
	wsSrv.connections.Range(func(key, value any) bool {
		checkedServer = true
		c := key.(*Connection)
		m := value.(*ConnectionMetadata)
		if m.RemoteAddr == "" {
			t.Fatal("expected metadata")
		}
		typ, msg, err := c.Read()
		if err != nil {
			t.Fatal("server failed to read", err)
		}
		if typ != websocket.TextMessage {
			t.Fatal("unexpected type", typ)
		}
		if string(msg) != "hello world" {
			t.Fatal("unexpected message", string(msg))
		}
		err = c.Write(websocket.TextMessage, []byte("server says hi"))
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
	typ, msg, err := conn.Read()
	if err != nil {
		t.Fatal("server failed to read", err)
	}
	if typ != websocket.TextMessage {
		t.Fatal("unexpected type", typ)
	}
	if string(msg) != "server says hi" {
		t.Fatal("unexpected message", string(msg))
	}
	_, _, _ = conn.Read()
	ctx := conn.CloseCtx()
	cause := context.Cause(ctx)
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
