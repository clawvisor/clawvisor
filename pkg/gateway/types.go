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
	SessionID string         `json:"session_id,omitempty"`
}

// RequestContext carries metadata about the agent's session.
type RequestContext struct {
	Source      string  `json:"source"`
	DataOrigin  *string `json:"data_origin"`
	CallbackURL string  `json:"callback_url"`
}

// BatchRequest is the payload sent by an agent to POST /api/gateway/batch.
// Each sub-request is handled by the same single-request pipeline (auth,
// task scope, intent verification, audit), and results are returned in the
// same order. A sub-request failure never fails the whole batch — each
// sub-result carries its own status/code.
type BatchRequest struct {
	Requests []Request `json:"requests"`
}

// BatchResponse is the aggregated response from POST /api/gateway/batch.
// Each entry mirrors the shape of a single-request response (status,
// request_id, result/error, code, etc.), preserved as-is so clients can
// reuse the same parser for both endpoints.
type BatchResponse struct {
	Results []map[string]any `json:"results"`
}
