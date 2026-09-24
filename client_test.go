package websocket

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"
)

// listenSilent accepts TCP connections that never answer the handshake, and returns the websocket URL.
func listenSilent(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var conns []net.Conn
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, conn)
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, conn := range conns {
			_ = conn.Close()
		}
	})
	return "ws://" + ln.Addr().String()
}

func TestDialAbort(t *testing.T) {
	t.Parallel()
	t.Run("cancel", func(t *testing.T) {
		t.Parallel()
		url := listenSilent(t)
		ctx, cancel := context.WithCancel(t.Context())
		time.AfterFunc(50*time.Millisecond, cancel)
		result := make(chan error, 1)
		go func() {
			_, err := Dial(ctx, url)
			result <- err
		}()
		if err := await(t, result); !errors.Is(err, context.Canceled) {
			t.Fatal("expected canceled dial", err)
		}
	})
	t.Run("handshake timeout", func(t *testing.T) {
		t.Parallel()
		url := listenSilent(t)
		result := make(chan error, 1)
		go func() {
			_, err := Dial(t.Context(), url, WithHandshakeTimeout(50*time.Millisecond))
			result <- err
		}()
		if err := await(t, result); err == nil {
			t.Fatal("expected handshake timeout")
		}
	})
	t.Run("client close", func(t *testing.T) {
		t.Parallel()
		rc := NewClient(listenSilent(t))
		result := make(chan error, 1)
		go func() {
			result <- rc.Write(TextMessage, nil)
		}()
		time.Sleep(50 * time.Millisecond)
		if err := rc.Close(); err != nil {
			t.Fatal("failed to close", err)
		}
		if err := await(t, result); !errors.Is(err, ErrNotReconnecting) {
			t.Fatal("expected aborted dial", err)
		}
	})
}

// TestDialContextAfterDial checks that the context of Dial does not affect the connection after the dial.
func TestDialContextAfterDial(t *testing.T) {
	t.Parallel()
	srv := NewServer(func(c *Connection, meta *ConnectionMetadata) (struct{}, error) {
		go func() {
			typ, data, err := c.Read()
			if err == nil {
				_ = c.Write(typ, data)
			}
		}()
		return struct{}{}, nil
	})
	ctx, cancel := context.WithCancel(t.Context())
	conn, err := Dial(ctx, serve(t, srv))
	if err != nil {
		t.Fatal("failed to dial", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	cancel()
	if err := conn.Write(TextMessage, []byte("echo")); err != nil {
		t.Fatal("failed to write", err)
	}
	msg := await(t, readAsync(conn))
	if msg.err != nil || string(msg.data) != "echo" {
		t.Fatal("unexpected echo", string(msg.data), msg.err)
	}
}

func TestDialHeader(t *testing.T) {
	t.Parallel()
	headers := make(chan http.Header, 2)
	srv := NewServer(func(c *Connection, meta *ConnectionMetadata) (struct{}, error) {
		if meta.Origin != meta.Header.Get("Origin") {
			t.Error("unexpected origin", meta.Origin)
		}
		headers <- meta.Header
		return struct{}{}, nil
	}, WithCheckOrigin[struct{}](func(r *http.Request) bool {
		return r.Header.Get("Origin") == "https://app.example"
	}))
	url := serve(t, srv)
	header := WithHeader(http.Header{
		"Authorization": {"Bearer secret"},
		"origin":        {"https://app.example"},
	})
	check := func(t *testing.T) {
		h := await(t, headers)
		if got := h.Get("Authorization"); got != "Bearer secret" {
			t.Fatal("unexpected authorization", got)
		}
	}
	t.Run("dial", func(t *testing.T) {
		dial(t, url, header)
		check(t)
	})
	t.Run("client", func(t *testing.T) {
		rc := NewClient(url, header)
		t.Cleanup(func() { _ = rc.Close() })
		if err := rc.Write(TextMessage, nil); err != nil {
			t.Fatal("failed to write", err)
		}
		check(t)
	})
	t.Run("rejected origin", func(t *testing.T) {
		_, err := Dial(t.Context(), url)
		if err == nil {
			t.Fatal("expected origin check to fail")
		}
	})
	t.Run("reserved", func(t *testing.T) {
		_, err := Dial(t.Context(), url, WithHeader(http.Header{"sec-websocket-key": {"x"}}))
		if err == nil {
			t.Fatal("expected reserved header to fail")
		}
	})
}

func TestClientCloseNeverConnected(t *testing.T) {
	t.Parallel()
	rc := NewClient("ws://127.0.0.1:1/never")
	if err := rc.Close(); err != nil {
		t.Fatal("failed to close", err)
	}
	if err := rc.Err(); !errors.Is(err, ErrNotConnected) {
		t.Fatal("expected not connected", err)
	}
	if err := rc.Write(TextMessage, nil); !errors.Is(err, ErrNotReconnecting) {
		t.Fatal("expected not reconnecting", err)
	}
}
