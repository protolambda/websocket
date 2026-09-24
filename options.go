package websocket

import (
	"net/http"
	"net/url"
	"time"
)

const (
	defaultPingInterval     = 30 * time.Second
	defaultPongTimeout      = 30 * time.Second
	defaultWriteTimeout     = 10 * time.Second
	defaultCloseTimeout     = time.Second
	defaultHandshakeTimeout = 45 * time.Second
	defaultReadLimit        = 32 * 1024 * 1024
)

// connConfig holds the settings of a Connection.
type connConfig struct {
	pingInterval time.Duration
	pongTimeout  time.Duration
	writeTimeout time.Duration
	closeTimeout time.Duration
	readLimit    int64
	compression  bool
}

func defaultConnConfig() connConfig {
	return connConfig{
		pingInterval: defaultPingInterval,
		pongTimeout:  defaultPongTimeout,
		writeTimeout: defaultWriteTimeout,
		closeTimeout: defaultCloseTimeout,
		readLimit:    defaultReadLimit,
	}
}

// ConnOpt configures a Connection.
// It can be passed to Dial and NewClient directly, and to NewServer with WithConnOpts.
type ConnOpt func(c *connConfig)

func (o ConnOpt) applyDial(c *dialConfig) {
	o(&c.conn)
}

// WithPingInterval sets how long the connection may receive nothing before it sends a ping to the peer.
// Zero or negative disables pings, and with them the pong timeout. The default is 30 seconds.
func WithPingInterval(d time.Duration) ConnOpt {
	return func(c *connConfig) {
		c.pingInterval = max(d, 0)
	}
}

// WithPongTimeout sets how long a Read waits for the peer to answer a ping, in addition to the ping interval:
// a Read fails with ErrPongTimeout, and closes the connection, if no message, ping or pong is received for
// the ping interval plus the pong timeout. Zero or negative disables the timeout, but not the pings. The default is 30 seconds.
func WithPongTimeout(d time.Duration) ConnOpt {
	return func(c *connConfig) {
		c.pongTimeout = max(d, 0)
	}
}

// WithWriteTimeout bounds the writing of each message and ping.
// Zero or negative disables the timeout. The default is 10 seconds.
func WithWriteTimeout(d time.Duration) ConnOpt {
	return func(c *connConfig) {
		c.writeTimeout = max(d, 0)
	}
}

// WithCloseTimeout bounds the writing of the close message, when the connection is closed gracefully.
// A peer that does not read delays each close by this timeout.
// Zero or negative closes without a close message. The default is 1 second.
func WithCloseTimeout(d time.Duration) ConnOpt {
	return func(c *connConfig) {
		c.closeTimeout = max(d, 0)
	}
}

// WithReadLimit sets the maximum size of a message from the peer, in bytes.
// A larger message fails the Read with websocket.ErrReadLimit (of the Gorilla library), and closes the connection.
// Zero or negative removes the limit. The default is 32 MiB.
func WithReadLimit(limit int64) ConnOpt {
	return func(c *connConfig) {
		c.readLimit = max(limit, 0)
	}
}

// WithCompression enables or disables per-message compression (RFC 7692), which is used if both peers enable it.
// Dial and NewClient enable it by default; a Server disables it by default.
// The Gorilla library marks its compression support as experimental.
func WithCompression(enabled bool) ConnOpt {
	return func(c *connConfig) {
		c.compression = enabled
	}
}

// dialConfig holds the settings of Dial.
type dialConfig struct {
	conn             connConfig
	header           http.Header
	handshakeTimeout time.Duration
	proxy            func(req *http.Request) (*url.URL, error)
}

func newDialConfig(opts []DialOpt) dialConfig {
	cfg := dialConfig{
		conn:             defaultConnConfig(),
		handshakeTimeout: defaultHandshakeTimeout,
	}
	cfg.conn.compression = true
	for _, opt := range opts {
		opt.applyDial(&cfg)
	}
	return cfg
}

// DialOpt configures Dial and NewClient. Every ConnOpt is a DialOpt.
type DialOpt interface {
	applyDial(c *dialConfig)
}

type dialOptFn func(c *dialConfig)

func (fn dialOptFn) applyDial(c *dialConfig) {
	fn(c)
}

// WithHeader adds the fields of h to the headers of the handshake request, e.g. Origin, Authorization or User-Agent.
// A field replaces the values of the same field from an earlier WithHeader.
// The Upgrade and Connection fields, and the Sec-WebSocket-* fields other than Sec-WebSocket-Protocol,
// are set by the handshake itself: Dial fails if h has one of them.
func WithHeader(h http.Header) DialOpt {
	h = h.Clone()
	return dialOptFn(func(c *dialConfig) {
		if c.header == nil {
			c.header = make(http.Header, len(h))
		}
		for k, vs := range h {
			c.header[http.CanonicalHeaderKey(k)] = vs
		}
	})
}

// WithProxy makes Dial connect through a proxy, e.g. to reach the endpoint from a network that requires one,
// or to hide the network address of the client from the endpoint.
// By default, Dial connects directly, and ignores the proxy environment variables.
//
// fn is called for every dial, with the handshake request. Its URL has the scheme http for a ws:// endpoint,
// and https for a wss:// endpoint. fn returns the URL of the proxy, or nil to connect directly. For example:
//   - http.ProxyFromEnvironment uses the proxy of the HTTP_PROXY or HTTPS_PROXY environment variable,
//     unless NO_PROXY excludes the host. It never proxies localhost or loopback addresses,
//     and reads the environment only once per process.
//   - http.ProxyURL(u) uses the proxy u for every endpoint.
//
// The proxy URL scheme may be http, for a tunnel with HTTP CONNECT (with basic authentication if the URL has
// a user and password), or socks5, which resolves host names at the proxy. Other schemes fail the dial.
// The proxy sees the host and port of the endpoint. With a ws:// endpoint it can also read and change
// the messages; with a wss:// endpoint, TLS runs through the tunnel, between the client and the endpoint.
// A nil fn connects directly.
func WithProxy(fn func(req *http.Request) (*url.URL, error)) DialOpt {
	return dialOptFn(func(c *dialConfig) {
		c.proxy = fn
	})
}

// WithHandshakeTimeout bounds the handshake: connecting, TLS, and the HTTP upgrade.
// Zero or negative disables the timeout; the context of Dial still applies. The default is 45 seconds.
func WithHandshakeTimeout(d time.Duration) DialOpt {
	return dialOptFn(func(c *dialConfig) {
		c.handshakeTimeout = max(d, 0)
	})
}
