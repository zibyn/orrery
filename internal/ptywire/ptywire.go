// Package ptywire is the WebSocket framing shared by the Agent and the CLI for
// a PTY Attachment: binary frames are terminal bytes, text frames are controls
// (api.md §4).
package ptywire

import "golang.org/x/net/websocket"

// Frame keeps the payload type, which websocket.Message.Receive discards.
type Frame struct {
	Text bool
	Data []byte
}

var Codec = websocket.Codec{
	Marshal: func(v interface{}) ([]byte, byte, error) {
		switch data := v.(type) {
		case []byte:
			return data, websocket.BinaryFrame, nil
		case string:
			return []byte(data), websocket.TextFrame, nil
		}
		return nil, websocket.UnknownFrame, websocket.ErrNotSupported
	},
	Unmarshal: func(data []byte, payloadType byte, v interface{}) error {
		f := v.(*Frame)
		f.Text = payloadType == websocket.TextFrame
		f.Data = data
		return nil
	},
}
