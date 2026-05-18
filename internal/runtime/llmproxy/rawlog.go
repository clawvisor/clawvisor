package llmproxy

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// RawIOLogger captures full raw HTTP bodies on both legs of every
// LLM call through the lite-proxy. Three capture phases:
//
//   - "proxy_received_request" — the body the harness sent us before
//     preprocess rewrites. This is an exact request-side capture after
//     HTTP read/decode but before Clawvisor mutates the conversation.
//   - "inbound_request"   — the body we send upstream after preprocess
//     rewrites (task-prompt rewrite, inline approval rewrite, control
//     notice injection, stable secret replay). This is what the upstream
//     LLM provider sees.
//   - "upstream_response" — the body the LLM provider sent back to us
//     (post-decompression, since we force `Accept-Encoding: identity`).
//   - "harness_response"  — the body we send back to the harness after
//     postprocess (tool_use rewrites, substitutions, intercepts).
//
// Together these cover everything that enters or leaves the LLM, plus
// what the harness sees. Diagnosing model loops requires knowing both
// what the proxy received and what the model actually receives —
// guessing at conversation state from summaries has limits.
//
// Disabled by default. Operators enable by setting
// CLAWVISOR_PROXY_LITE_RAW_LOG to a file path. Production should keep
// this off — bodies contain prompts, tool outputs, and credentials in
// the model's conversation history. (Autovault placeholders are in
// there. Live autovault placeholders are redacted before writing; real
// bearer tokens should not enter conversation state because the resolver
// path replaces them with nonces, but the raw log may still contain
// credential-shaped prompt content, user files, etc.
//
// A nil receiver is a no-op so callers don't need a branch at every
// site.
type RawIOLogger struct {
	mu       sync.Mutex
	w        io.Writer
	now      func() time.Time
	prefixes map[string]rawPromptPrefix
}

type rawPromptPrefix struct {
	SHA256 string
	Bytes  int
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
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("lite-proxy: chmod raw-io log %q: %w", path, err)
	}
	return NewRawIOLogger(f), nil
}

// NewRawIOLogger wraps an existing io.Writer. Useful in tests where a
// bytes.Buffer stands in for the file.
func NewRawIOLogger(w io.Writer) *RawIOLogger {
	return &RawIOLogger{w: w, now: time.Now, prefixes: map[string]rawPromptPrefix{}}
}

// RawIOEvent is the payload written per capture point.
type RawIOEvent struct {
	// Phase is one of "proxy_received_request" / "inbound_request" /
	// "upstream_response" / "harness_response". Filter on this to slice
	// the log.
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
	if ev.BodyEncoding == "" {
		ev.Body = redactRawLogLivePlaceholders(ev.Body)
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
	l.mu.Lock()
	defer l.mu.Unlock()
	l.addPromptPrefixFieldsLocked(payload, ev)
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
	_, _ = l.w.Write(line)
	_, _ = l.w.Write([]byte{'\n'})
}

func (l *RawIOLogger) addPromptPrefixFieldsLocked(payload map[string]any, ev RawIOEvent) {
	if ev.Phase != "inbound_request" || ev.Body == "" || ev.BodyEncoding != "" {
		return
	}
	prefix, model, ok := rawPromptPrefixBytes(ev.Provider, []byte(ev.Body))
	if !ok || len(prefix) == 0 {
		return
	}
	sum := sha256.Sum256(prefix)
	digest := hex.EncodeToString(sum[:])
	payload["prompt_prefix_sha256"] = digest
	payload["prompt_prefix_bytes"] = len(prefix)
	key := strings.Join([]string{ev.UserID, ev.AgentID, ev.Provider, ev.Method, ev.Path, model}, "\x00")
	payload["prompt_prefix_cache_key"] = strings.Join([]string{ev.AgentID, ev.Provider, ev.Path, model}, "|")
	if l.prefixes == nil {
		l.prefixes = map[string]rawPromptPrefix{}
	}
	if prev, ok := l.prefixes[key]; ok {
		payload["prompt_prefix_stable_with_previous"] = prev.SHA256 == digest && prev.Bytes == len(prefix)
		if prev.SHA256 != digest {
			payload["prompt_prefix_previous_sha256"] = prev.SHA256
			payload["prompt_prefix_previous_bytes"] = prev.Bytes
		}
	}
	l.prefixes[key] = rawPromptPrefix{SHA256: digest, Bytes: len(prefix)}
}

func rawPromptPrefixBytes(provider string, body []byte) ([]byte, string, bool) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, "", false
	}
	model := rawJSONScalarString(raw["model"])
	var parts [][]byte
	switch strings.ToLower(provider) {
	case "anthropic":
		parts = appendRawPart(parts, "system", raw["system"])
	case "openai":
		parts = appendRawPart(parts, "instructions", raw["instructions"])
		parts = appendOpenAISystemMessages(parts, raw["messages"])
	default:
		return nil, model, false
	}
	parts = appendRawPart(parts, "tools", raw["tools"])
	if len(parts) == 0 {
		return nil, model, false
	}
	return bytes.Join(parts, []byte{'\n'}), model, true
}

func appendOpenAISystemMessages(parts [][]byte, raw json.RawMessage) [][]byte {
	if len(raw) == 0 || string(raw) == "null" {
		return parts
	}
	var messages []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &messages); err != nil {
		return parts
	}
	for _, msg := range messages {
		role := rawJSONScalarString(msg["role"])
		if role != "system" && role != "developer" {
			break
		}
		parts = appendRawPart(parts, "message:"+role, msg["content"])
	}
	return parts
}

func appendRawPart(parts [][]byte, label string, raw json.RawMessage) [][]byte {
	if len(raw) == 0 || string(raw) == "null" {
		return parts
	}
	part := make([]byte, 0, len(label)+1+len(raw))
	part = append(part, label...)
	part = append(part, ':')
	part = append(part, raw...)
	return append(parts, part)
}

func rawJSONScalarString(raw json.RawMessage) string {
	var s string
	_ = json.Unmarshal(raw, &s)
	return s
}

// EmitRaw writes an arbitrary JSON object as one line. Used by
// streaming-progress instrumentation that doesn't fit the RawIOEvent
// schema — these are short, diagnostic-only records, not body
// captures. A timestamp is always added; the caller's fields override
// anything except `timestamp`.
func (l *RawIOLogger) EmitRaw(fields map[string]any) {
	if l == nil || l.w == nil {
		return
	}
	if fields == nil {
		fields = map[string]any{}
	}
	fields["timestamp"] = l.now().UTC().Format(time.RFC3339Nano)
	line, err := json.Marshal(fields)
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
		return redactRawLogLivePlaceholders(string(body)), ""
	}
	return base64.StdEncoding.EncodeToString(body), "base64"
}

var rawLogLivePlaceholderRE = regexp.MustCompile(`(?i)(^|[^a-z0-9])(autovault[_:][a-z0-9._:-]+)`)

func redactRawLogLivePlaceholders(s string) string {
	if s == "" {
		return s
	}
	return rawLogLivePlaceholderRE.ReplaceAllString(s, "${1}<REDACTED:autovault>")
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
