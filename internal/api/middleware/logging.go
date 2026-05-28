package middleware

import (
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"time"

	"github.com/clawvisor/clawvisor/pkg/cloudlogging"
)

type responseWriter struct {
	http.ResponseWriter
	status int
}

func (rw *responseWriter) WriteHeader(status int) {
	rw.status = status
	rw.ResponseWriter.WriteHeader(status)
}

// Flush implements http.Flusher so SSE and other streaming handlers work
// through the logging middleware.
func (rw *responseWriter) Flush() {
	if f, ok := rw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the underlying ResponseWriter
// (needed for SetWriteDeadline on SSE connections).
func (rw *responseWriter) Unwrap() http.ResponseWriter {
	return rw.ResponseWriter
}

// Logging logs method, path, status, duration, and any fields accumulated by
// handlers via AddLogField. It also sets X-Trace-Id on every response.
func Logging(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()

			// Prefer an upstream trace from X-Cloud-Trace-Context or W3C
			// traceparent so log entries coalesce under the GCP request
			// log in the Cloud Logs Explorer. Fall back to a locally
			// generated ID so X-Trace-Id is always set.
			traceID, spanID := cloudlogging.ExtractTrace(r)
			if traceID == "" {
				traceID = generateTraceID()
			}
			w.Header().Set("X-Trace-Id", traceID)

			ctx := cloudlogging.WithTrace(r.Context(), traceID, spanID)
			ctx, lf := WithLogFields(ctx)
			r = r.WithContext(ctx)

			rw := &responseWriter{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rw, r)

			attrs := []slog.Attr{
				slog.String("trace_id", traceID),
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", rw.status),
				slog.Int64("duration_ms", time.Since(start).Milliseconds()),
				slog.String("remote_addr", r.RemoteAddr),
			}
			attrs = append(attrs, lf.Attrs()...)

			// Convert []slog.Attr to []any for LogAttrs.
			logger.LogAttrs(r.Context(), slog.LevelInfo, "request", attrs...)
		})
	}
}

func generateTraceID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
