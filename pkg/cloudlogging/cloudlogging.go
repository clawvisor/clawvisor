// Package cloudlogging adapts a slog handler for Google Cloud Logging, so
// log entries that share a trace ID coalesce under the parent request entry
// in the Logs Explorer.
//
// The Logs Explorer groups child entries beneath the request log when they
// carry a matching logging.googleapis.com/trace field. This package supplies
// an slog.Handler wrapper that injects that field (and spanId, when known)
// from values placed on the context by the HTTP middleware.
package cloudlogging

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
)

// Field names recognised by Google Cloud Logging.
const (
	traceField  = "logging.googleapis.com/trace"
	spanIDField = "logging.googleapis.com/spanId"
)

type ctxKey struct{}

type traceCtx struct {
	traceID string
	spanID  string
}

// WithTrace returns a context carrying the given trace and span IDs. An empty
// traceID returns ctx unchanged.
func WithTrace(ctx context.Context, traceID, spanID string) context.Context {
	if traceID == "" {
		return ctx
	}
	return context.WithValue(ctx, ctxKey{}, traceCtx{traceID: traceID, spanID: spanID})
}

// TraceFromContext returns the trace and span IDs stored on ctx, if any.
func TraceFromContext(ctx context.Context) (traceID, spanID string, ok bool) {
	if ctx == nil {
		return "", "", false
	}
	tc, ok := ctx.Value(ctxKey{}).(traceCtx)
	if !ok {
		return "", "", false
	}
	return tc.traceID, tc.spanID, true
}

// ExtractTrace parses the trace ID and span ID from an incoming request.
// It prefers the Google Cloud-specific X-Cloud-Trace-Context header and falls
// back to the W3C traceparent header. Returns empty strings if neither is
// present or parseable.
func ExtractTrace(r *http.Request) (traceID, spanID string) {
	if v := r.Header.Get("X-Cloud-Trace-Context"); v != "" {
		// Format: TRACE_ID/SPAN_ID;o=OPTIONS
		rest := v
		if i := strings.Index(rest, ";"); i >= 0 {
			rest = rest[:i]
		}
		if i := strings.Index(rest, "/"); i >= 0 {
			return rest[:i], rest[i+1:]
		}
		return rest, ""
	}
	if v := r.Header.Get("traceparent"); v != "" {
		// Format: VERSION-TRACE_ID-PARENT_ID-FLAGS
		parts := strings.Split(v, "-")
		if len(parts) >= 3 {
			return parts[1], parts[2]
		}
	}
	return "", ""
}

// Handler wraps another slog.Handler and adds trace fields recognised by
// Google Cloud Logging when a trace is present on the record's context.
type Handler struct {
	inner     slog.Handler
	projectID string
}

// NewHandler returns a Handler that augments inner with GCP trace fields.
// projectID is the Google Cloud project ID (the trace field must be a fully
// qualified name like "projects/PROJECT_ID/traces/TRACE_ID"). If projectID
// is empty, the handler still emits the bare trace ID so entries correlate
// in other viewers, but they will not group in the Cloud Logs Explorer.
func NewHandler(inner slog.Handler, projectID string) *Handler {
	return &Handler{inner: inner, projectID: projectID}
}

// Enabled reports whether the inner handler accepts records at the given level.
func (h *Handler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

// Handle injects trace fields from ctx and forwards to the inner handler.
func (h *Handler) Handle(ctx context.Context, r slog.Record) error {
	traceID, spanID, ok := TraceFromContext(ctx)
	if !ok || traceID == "" {
		return h.inner.Handle(ctx, r)
	}
	traceValue := traceID
	if h.projectID != "" {
		traceValue = fmt.Sprintf("projects/%s/traces/%s", h.projectID, traceID)
	}
	r.AddAttrs(slog.String(traceField, traceValue))
	if spanID != "" {
		r.AddAttrs(slog.String(spanIDField, spanID))
	}
	return h.inner.Handle(ctx, r)
}

// WithAttrs returns a new Handler whose inner handler has the given attributes.
func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &Handler{inner: h.inner.WithAttrs(attrs), projectID: h.projectID}
}

// WithGroup returns a new Handler whose inner handler is in the given group.
func (h *Handler) WithGroup(name string) slog.Handler {
	return &Handler{inner: h.inner.WithGroup(name), projectID: h.projectID}
}
