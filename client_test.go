package websocket

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
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

// serveEcho serves connections that echo every message, and returns the websocket URL.
func serveEcho(t *testing.T) string {
	t.Helper()
	srv := NewServer(func(c *Connection, meta *ConnectionMetadata) (struct{}, error) {
		go func() {
			for {
				typ, data, err := c.Read()
				if err != nil || c.Write(typ, data) != nil {
					return
				}
			}
		}()
		return struct{}{}, nil
	})
	return serve(t, srv)
}

// serveConnectProxy serves an HTTP CONNECT proxy, and returns its URL and the number of tunnels it opened.
func serveConnectProxy(t *testing.T) (*url.URL, *atomic.Int64) {
	t.Helper()
	var tunnels atomic.Int64
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			http.Error(w, "CONNECT only", http.StatusMethodNotAllowed)
			return
		}
		target, err := net.Dial("tcp", r.Host)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer target.Close()
		client, rw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			t.Error("failed to hijack", err)
			return
		}
		defer client.Close()
		tunnels.Add(1)
		if _, err := io.WriteString(client, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
			return
		}
		done := make(chan struct{}, 2)
		go func() {
			_, _ = io.Copy(target, rw.Reader)
			done <- struct{}{}
		}()
		go func() {
			_, _ = io.Copy(client, target)
			done <- struct{}{}
		}()
		<-done
	}))
	t.Cleanup(proxy.Close)
	proxyURL, err := url.Parse(proxy.URL)
	if err != nil {
		t.Fatal(err)
	}
	return proxyURL, &tunnels
}

func TestProxy(t *testing.T) {
	t.Parallel()
	proxyURL, tunnels := serveConnectProxy(t)
	var scheme atomic.Value
	conn := dial(t, serveEcho(t), WithProxy(func(req *http.Request) (*url.URL, error) {
		scheme.Store(req.URL.Scheme)
		return proxyURL, nil
	}))
	if err := conn.Write(TextMessage, []byte("via proxy")); err != nil {
		t.Fatal("failed to write", err)
	}
	msg := await(t, readAsync(conn))
	if msg.err != nil || string(msg.data) != "via proxy" {
		t.Fatal("unexpected echo", string(msg.data), msg.err)
	}
	if n := tunnels.Load(); n != 1 {
		t.Fatal("expected one tunnel", n)
	}
	if got := scheme.Load(); got != "http" {
		t.Fatal("expected http scheme for a ws endpoint", got)
	}
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
	t.Run("proxy", func(t *testing.T) {
		t.Parallel()
		proxyURL := &url.URL{Scheme: "http", Host: strings.TrimPrefix(listenSilent(t), "ws://")}
		ctx, cancel := context.WithCancel(t.Context())
		time.AfterFunc(50*time.Millisecond, cancel)
		result := make(chan error, 1)
		go func() {
			_, err := Dial(ctx, "ws://endpoint.invalid", WithProxy(http.ProxyURL(proxyURL)))
			result <- err
		}()
		if err := await(t, result); !errors.Is(err, context.Canceled) {
			t.Fatal("expected canceled dial", err)
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
	ctx, cancel := context.WithCancel(t.Context())
	conn, err := Dial(ctx, serveEcho(t))
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
