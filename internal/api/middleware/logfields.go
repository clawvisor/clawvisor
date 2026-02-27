package middleware

import (
	"context"
	"log/slog"
	"sync"
)

type logFieldsKey struct{}

// LogFields is a thread-safe accumulator for slog attributes that are
// collected during request processing and flushed in the logging middleware.
type LogFields struct {
	mu    sync.Mutex
	attrs []slog.Attr
}

// Add appends a key-value pair to the accumulated fields.
func (lf *LogFields) Add(key string, value any) {
	lf.mu.Lock()
	lf.attrs = append(lf.attrs, slog.Any(key, value))
	lf.mu.Unlock()
}

// Attrs returns a snapshot of the accumulated attributes.
func (lf *LogFields) Attrs() []slog.Attr {
	lf.mu.Lock()
	defer lf.mu.Unlock()
	out := make([]slog.Attr, len(lf.attrs))
	copy(out, lf.attrs)
	return out
}

// WithLogFields stores a new LogFields in the context and returns both.
func WithLogFields(ctx context.Context) (context.Context, *LogFields) {
	lf := &LogFields{}
	return context.WithValue(ctx, logFieldsKey{}, lf), lf
}

// AddLogField appends a field to the LogFields in ctx. No-op if ctx has none.
func AddLogField(ctx context.Context, key string, value any) {
	if lf, ok := ctx.Value(logFieldsKey{}).(*LogFields); ok {
		lf.Add(key, value)
	}
}
