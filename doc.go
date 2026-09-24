// Package websocket is an opinionated wrapper around the Gorilla websocket library.
//
// A Connection handles pings, pongs and close messages, closes on any read or write error,
// and reports why it closed. Dial opens a client connection, Client reconnects lazily,
// and Server accepts connections and keeps the set of open connections, with a value for each.
package websocket
