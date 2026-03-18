package relay

import "encoding/json"

// FrameType identifies the message type on the WebSocket tunnel.
type FrameType string

const (
	FrameHTTPRequest  FrameType = "http_request"
	FrameHTTPResponse FrameType = "http_response"
	FramePing         FrameType = "ping"
	FramePong         FrameType = "pong"
)

// Frame is the envelope for all WebSocket messages.
type Frame struct {
	Type    FrameType       `json:"type"`
	ID      string          `json:"id,omitempty"`
	Payload json.RawMessage `json:"payload"`
}

// HTTPRequestPayload is sent from relay → daemon.
type HTTPRequestPayload struct {
	Method  string              `json:"method"`
	Path    string              `json:"path"`
	Headers map[string][]string `json:"headers"`
	Body    string              `json:"body"` // base64-encoded
}

// HTTPResponsePayload is sent from daemon → relay.
type HTTPResponsePayload struct {
	Status  int                 `json:"status"`
	Headers map[string][]string `json:"headers"`
	Body    string              `json:"body"` // base64-encoded
}
