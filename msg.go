package websocket

import "fmt"

// MessageType is a type-safe enum, replacing the message-types by the underlying gorilla-websocket library.
type MessageType uint

func (typ MessageType) String() string {
	switch typ {
	case TextMessage:
		return "TEXT"
	case BinaryMessage:
		return "BINARY"
	case CloseMessage:
		return "CLOSE"
	case PingMessage:
		return "PING"
	case PongMessage:
		return "PONG"
	default:
		return fmt.Sprintf("unknown-type-%d", uint(typ))
	}
}

// The message types are defined in RFC 6455, section 11.8.
const (
	// TextMessage denotes a text data message. The text message payload is
	// interpreted as UTF-8 encoded text data.
	TextMessage MessageType = 1

	// BinaryMessage denotes a binary data message.
	BinaryMessage MessageType = 2

	// CloseMessage denotes a close control message. The optional message
	// payload contains a numeric code and text. Use the FormatCloseMessage
	// function to format a close message payload.
	CloseMessage MessageType = 8

	// PingMessage denotes a ping control message. The optional message payload
	// is UTF-8 encoded text.
	PingMessage MessageType = 9

	// PongMessage denotes a pong control message. The optional message payload
	// is UTF-8 encoded text.
	PongMessage MessageType = 10
)

type Messenger interface {
	Write(messageType MessageType, data []byte) error
	Read() (messageType MessageType, p []byte, err error)
	Close() error
	Err() error
}
