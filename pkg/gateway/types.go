package gateway

// Request is the payload sent by an agent to POST /api/gateway/request.
type Request struct {
	Service   string         `json:"service"`
	Action    string         `json:"action"`
	Params    map[string]any `json:"params"`
	Reason    string         `json:"reason"`    // agent's stated reason (untrusted)
	Context   RequestContext `json:"context"`
	RequestID string         `json:"request_id"` // optional; generated if empty
	TaskID    string         `json:"task_id,omitempty"`
	BatchID   string         `json:"batch_id,omitempty"`
	SessionID string         `json:"session_id,omitempty"`
}

// RequestContext carries metadata about the agent's session.
type RequestContext struct {
	Source      string  `json:"source"`
	DataOrigin  *string `json:"data_origin"`
	CallbackURL string  `json:"callback_url"`
}
