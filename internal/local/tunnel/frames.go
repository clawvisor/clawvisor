package tunnel

import "encoding/json"

// FrameType identifies the type of a WebSocket frame.
type FrameType string

const (
	FrameAuth         FrameType = "auth"
	FrameCapabilities FrameType = "capabilities"
	FrameRequest      FrameType = "request"
	FrameResponse     FrameType = "response"
	FramePing         FrameType = "ping"
	FramePong         FrameType = "pong"
)

// Frame is the generic envelope for all WebSocket messages.
type Frame struct {
	Type    FrameType       `json:"type"`
	ID      string          `json:"id"`
	Payload json.RawMessage `json:"payload"`
}

// AuthPayload is sent as the first frame after WebSocket upgrade.
type AuthPayload struct {
	ConnectionToken string `json:"connection_token"`
}

// CapabilitiesPayload declares the daemon's available services.
type CapabilitiesPayload struct {
	Version  string            `json:"version"`
	Name     string            `json:"name"`
	Services []ServiceCapability `json:"services"`
}

// ServiceCapability describes a service for the cloud.
type ServiceCapability struct {
	ID          string             `json:"id"`
	Name        string             `json:"name"`
	Description string             `json:"description,omitempty"`
	Icon        string             `json:"icon,omitempty"`
	Actions     []ActionCapability `json:"actions"`
}

// ActionCapability describes an action for the cloud.
type ActionCapability struct {
	ID          string           `json:"id"`
	Name        string           `json:"name"`
	Description string           `json:"description,omitempty"`
	Params      []ParamCapability `json:"params"`
}

// ParamCapability describes a parameter for the cloud.
type ParamCapability struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	Required    bool   `json:"required"`
	Description string `json:"description,omitempty"`
}

// RequestPayload is sent from the cloud to invoke a local service action.
type RequestPayload struct {
	Service string            `json:"service"`
	Action  string            `json:"action"`
	Params  map[string]string `json:"params"`
}

// ResponsePayload is sent from the daemon to the cloud with the action result.
type ResponsePayload struct {
	Success bool            `json:"success"`
	Data    json.RawMessage `json:"data,omitempty"`
	Error   string          `json:"error,omitempty"`
}
