package llmproxy

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// RawIOLogger captures full raw HTTP bodies on both legs of every
// LLM call through the lite-proxy. Three capture phases:
//
//   - "inbound_request"   — the body the harness sent us (post any
//     preprocess rewrites we apply: task-prompt rewrite, inline
//     approval rewrite, control-notice injection). This is what the
//     upstream LLM provider sees.
//   - "upstream_response" — the body the LLM provider sent back to us
//     (post-decompression, since we force `Accept-Encoding: identity`).
//   - "harness_response"  — the body we send back to the harness after
//     postprocess (tool_use rewrites, substitutions, intercepts).
//
// Together these cover everything that enters or leaves the LLM, plus
// what the harness sees. Diagnosing model loops requires knowing what
// the model actually receives — guessing at conversation state from
// summaries has limits.
//
// Disabled by default. Operators enable by setting
// CLAWVISOR_PROXY_LITE_RAW_LOG to a file path. Production should keep
// this off — bodies contain prompts, tool outputs, and credentials in
// the model's conversation history. (Autovault placeholders are in
// there; real bearer tokens are not, since the resolver path replaces
// them with nonces before they enter conversation state — but the raw
// log will still contain credential-shaped prompt content, user files,
// etc.)
//
// A nil receiver is a no-op so callers don't need a branch at every
// site.
type RawIOLogger struct {
	mu  sync.Mutex
	w   io.Writer
	now func() time.Time
}

// OpenRawIOLogger opens path for append + create with mode 0600 so the
// raw bodies (which contain conversation content) are user-readable
// only. Empty path returns (nil, nil).
func OpenRawIOLogger(path string) (*RawIOLogger, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, nil
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("lite-proxy: open raw-io log %q: %w", path, err)
	}
	return NewRawIOLogger(f), nil
}

// NewRawIOLogger wraps an existing io.Writer. Useful in tests where a
// bytes.Buffer stands in for the file.
func NewRawIOLogger(w io.Writer) *RawIOLogger {
	return &RawIOLogger{w: w, now: time.Now}
}

// RawIOEvent is the payload written per capture point.
type RawIOEvent struct {
	// Phase is one of "inbound_request" / "upstream_response" /
	// "harness_response". Filter on this to slice the log.
	Phase string
	// RequestID correlates the three phases for one LLM call.
	RequestID string
	// UserID, AgentID, Provider — same fields the trace log carries.
	UserID   string
	AgentID  string
	Provider string
	// Method/Path — useful for spotting which endpoint
	// (messages/responses/chat).
	Method string
	Path   string
	// Status — populated on response phases.
	Status int
	// ContentType reflects the response Content-Type for the upstream
	// + harness phases.
	ContentType string
	// Headers is a subset of HTTP headers we capture (Auth, vendor
	// request id, content-length).
	Headers map[string]string
	// Body is the full bytes. Stored verbatim as string when valid
	// UTF-8; otherwise base64-encoded with BodyEncoding="base64".
	Body         string
	BodyEncoding string
	BodyBytes    int
	// Marker is a short tag callers can attach to label semantic
	// variants (e.g. "rewritten_for_inline_approve", "after_postprocess").
	Marker string
}

// Emit writes the event as one JSON line. Failures are silent —
// observability must not break the request path.
func (l *RawIOLogger) Emit(ev RawIOEvent) {
	if l == nil || l.w == nil {
		return
	}
	payload := map[string]any{
		"timestamp":    l.now().UTC().Format(time.RFC3339Nano),
		"phase":        ev.Phase,
		"request_id":   ev.RequestID,
		"user_id":      ev.UserID,
		"agent_id":     ev.AgentID,
		"provider":     ev.Provider,
		"method":       ev.Method,
		"path":         ev.Path,
		"status":       ev.Status,
		"content_type": ev.ContentType,
		"headers":      ev.Headers,
		"body_bytes":   ev.BodyBytes,
	}
	if ev.Marker != "" {
		payload["marker"] = ev.Marker
	}
	if ev.BodyEncoding != "" {
		payload["body_encoding"] = ev.BodyEncoding
	}
	if ev.Body != "" {
		payload["body"] = ev.Body
	}
	line, err := json.Marshal(payload)
	if err != nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	_, _ = l.w.Write(line)
	_, _ = l.w.Write([]byte{'\n'})
}

// EncodeBody returns (body, encoding) ready to drop into RawIOEvent.
// Valid UTF-8 (the common case — JSON, SSE) is stored as a string for
// easy `jq` traversal. Anything else gets base64-encoded so we don't
// produce broken JSON.
func EncodeBody(body []byte) (string, string) {
	if utf8.Valid(body) {
		return string(body), ""
	}
	return base64.StdEncoding.EncodeToString(body), "base64"
}

// SafeHeaderSnapshot pulls a small subset of headers worth keeping for
// correlation, dropping the bearer tokens we forward upstream. Returns
// nil when h is nil.
func SafeHeaderSnapshot(h http.Header) map[string]string {
	if h == nil {
		return nil
	}
	out := map[string]string{}
	for _, key := range []string{
		"Content-Type",
		"Content-Length",
		"Anthropic-Version",
		"Anthropic-Request-Id",
		"X-Request-Id",
		"Request-Id",
		"Openai-Organization",
		"Openai-Processing-Ms",
		"X-Stainless-Lang",
		"X-Stainless-Package-Version",
	} {
		v := h.Get(key)
		if v != "" {
			out[key] = v
		}
	}
	return out
}
