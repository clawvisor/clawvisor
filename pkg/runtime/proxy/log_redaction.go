package proxy

// Phase 0.X — log redaction policy.
//
// The runtime proxy sees decrypted traffic for every concurrent session.
// Logs that include request/response bodies, Authorization headers,
// query strings, or cookie values turn the log pipeline into a
// pseudo-decrypt-everything surface. This handler enforces a fixed
// allowlist of structured slog attribute keys; everything else is
// dropped.
//
// Use from cloud's binary wiring: wrap the slog handler at the top of
// main.go before passing the logger into NewServer / Manager.

import (
	"context"
	"log/slog"
	"strings"
)

// AllowedLogKeys is the fixed set of structured fields that may appear
// in runtime-proxy log records. ANYTHING NOT IN THIS SET IS REDACTED.
//
// Adding a key requires a security review — see the on-call appendix.
var AllowedLogKeys = map[string]struct{}{
	// Identity (no secrets):
	"session_id":  {},
	"user_id":     {},
	"agent_id":    {},
	"org_id":      {},
	"request_id":  {},
	"approval_id": {},
	"task_id":     {},
	"lease_id":    {},
	"tool_use_id": {},

	// Request shape (no bodies, no query, no headers):
	"method":         {},
	"host":           {},
	"path":           {},
	"upstream_host":  {},
	"upstream_path":  {},
	"req_bytes":      {},
	"resp_bytes":     {},
	"status":         {},
	"latency_ms":     {},
	"verdict":        {},
	"hook":           {},
	"event_type":     {},
	"action_kind":    {},
	"decision":       {},
	"outcome":        {},
	"observation":    {},
	"reason_kind":    {},
	"resolution_kind": {},
	"matched_rule":   {},

	// Errors (treated as opaque strings, not bodies):
	"err":          {},
	"error":        {},
	"err_class":    {},

	// Context / observability metadata:
	"phase":        {},
	"component":    {},
	"region":       {},
	"pod":          {},
	"replica":      {},
	"version":      {},
	"build_id":     {},
}

// RedactingHandler wraps an slog.Handler and drops any attribute whose
// key is not in AllowedLogKeys. Group prefixes are preserved. The
// message itself is pass-through (so callers must already have phrased
// it free of sensitive content).
type RedactingHandler struct {
	wrapped slog.Handler
	allowed map[string]struct{}
}

// NewRedactingHandler returns a handler that filters attributes against
// the proxy log allowlist.
func NewRedactingHandler(wrapped slog.Handler) *RedactingHandler {
	if wrapped == nil {
		return nil
	}
	return &RedactingHandler{wrapped: wrapped, allowed: AllowedLogKeys}
}

// NewRedactingHandlerWithAllowlist is for tests and for the Phase 1b
// per-component extension where a specific component (e.g., Vertex
// breaker) needs an additional allowed key.
func NewRedactingHandlerWithAllowlist(wrapped slog.Handler, allowed map[string]struct{}) *RedactingHandler {
	if wrapped == nil {
		return nil
	}
	return &RedactingHandler{wrapped: wrapped, allowed: allowed}
}

func (h *RedactingHandler) Enabled(ctx context.Context, lvl slog.Level) bool {
	return h.wrapped.Enabled(ctx, lvl)
}

func (h *RedactingHandler) Handle(ctx context.Context, r slog.Record) error {
	filtered := slog.NewRecord(r.Time, r.Level, r.Message, r.PC)
	r.Attrs(func(a slog.Attr) bool {
		if h.shouldKeep(a.Key) {
			filtered.AddAttrs(a)
		} else {
			filtered.AddAttrs(slog.String(a.Key, "<redacted>"))
		}
		return true
	})
	return h.wrapped.Handle(ctx, filtered)
}

func (h *RedactingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	kept := make([]slog.Attr, 0, len(attrs))
	for _, a := range attrs {
		if h.shouldKeep(a.Key) {
			kept = append(kept, a)
		} else {
			kept = append(kept, slog.String(a.Key, "<redacted>"))
		}
	}
	return &RedactingHandler{wrapped: h.wrapped.WithAttrs(kept), allowed: h.allowed}
}

func (h *RedactingHandler) WithGroup(name string) slog.Handler {
	return &RedactingHandler{wrapped: h.wrapped.WithGroup(name), allowed: h.allowed}
}

func (h *RedactingHandler) shouldKeep(key string) bool {
	if h == nil || h.allowed == nil {
		return false
	}
	// Defense in depth: never allow an attribute key that itself looks
	// like it carries secrets, even if the allowlist forgot to exclude it.
	lk := strings.ToLower(key)
	if strings.Contains(lk, "authorization") ||
		strings.Contains(lk, "cookie") ||
		strings.Contains(lk, "secret") ||
		strings.Contains(lk, "token") ||
		strings.Contains(lk, "api_key") ||
		strings.Contains(lk, "apikey") ||
		strings.Contains(lk, "password") ||
		strings.Contains(lk, "body") ||
		strings.Contains(lk, "payload") ||
		strings.Contains(lk, "query_string") {
		return false
	}
	_, ok := h.allowed[key]
	return ok
}
