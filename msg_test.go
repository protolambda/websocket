package websocket

import (
	"testing"

	"github.com/gorilla/websocket"
)

// TestMessageTypes just tests if the type-safe enum still matches upstream enum.
func TestMessageTypes(t *testing.T) {
	check := func(a MessageType, b int) {
		if b < 0 || MessageType(b) != a {
			t.Fatalf("message type broke: %d <> %d", a, b)
		}
	}
	check(TextMessage, websocket.TextMessage)
	check(BinaryMessage, websocket.BinaryMessage)
	check(CloseMessage, websocket.CloseMessage)
	check(PingMessage, websocket.PingMessage)
	check(PongMessage, websocket.PongMessage)
}
