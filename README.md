# websocket

Opinionated wrapper around [Gorilla Websocket](https://github.com/gorilla/websocket) that:
- Provides `Connection` for client and server that automatically:
  - Sends a ping when nothing was received from the peer for a while, and fails a `Read`
    when the peer does not answer in time (dead-peer detection happens while reading).
  - Closes on any read or write error, including a close message from the peer, a timeout, or an exceeded read limit.
  - Sends a close message when closed gracefully, within a close timeout.
  - Makes awaiting a connection-closure easy with a context, and close-reason with context-cause.
- Provides a `Server[E]` that maintains the set of active connections and their metadata (generic for customization),
  and closes them all on `Close`.
- Provides a `Dial` function to get a connection to an endpoint as client.
  The context bounds the whole dial, including the handshake.
- Provides a `Client` that handles reconnects.
- Provides Go typing for websocket message-types.
- Provides a `Messenger` interface for common client/connection message handling (read/write/close).

Options:
- Connection options, for `Dial`, `NewClient`, and `NewServer` (with `WithConnOpts`):
  `WithPingInterval`, `WithPongTimeout`, `WithWriteTimeout`, `WithCloseTimeout`, `WithReadLimit`, `WithCompression`.
- Dial options: `WithHeader` (e.g. `Origin` or `Authorization`), `WithHandshakeTimeout`,
  `WithProxy` (e.g. `http.ProxyFromEnvironment`, or `http.ProxyURL(u)` for an `http://` or `socks5://` proxy).
  `Dial` connects directly by default: it ignores the proxy environment variables unless `WithProxy` says otherwise.
- Server options: `WithOnDisconnect`, `WithCheckOrigin`, `WithOnUpgradeFailed`.

See the Go documentation of `Connection` for the concurrency and liveness contract.

## License

MIT, see [`LICENSE`](./LICENSE) file.
