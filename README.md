# websocket

Opinionated wrapper around [Gorilla Websocket](https://github.com/gorilla/websocket) that:
- Provides `Connection` for client and server that automatically:
  - Handles ping/pong, and resets ping schedule on read/write as necessary.
  - Handles close messages, and closes the connection.
  - Makes awaiting a connection-closure easy with a context, and close-reason with context-cause.
- Provides a `Server[E]` that maintains the set of active connections and their metadata (generic for customization).
- Provides a `Dial` function to get a connection to an endpoint as client.
- Provides a `Client` that handles reconnects.
- Provides Go typing for websocket message-types.
- Provides a `Messenger` interface for common client/connection message handling (read/write/close).

## License

MIT, see [`LICENSE`](./LICENSE) file.
