package middleware

import (
	"log/slog"
	"net/http"
	"runtime/debug"
)

// Recover returns middleware that catches handler panics, logs the panic with
// a stack trace, and returns a 500 JSON error to the client so a single
// bad request can't take down the server.
func Recover(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					// http.ErrAbortHandler is the documented way for handlers
					// to bail without logging — re-panic so the standard
					// library's own recover in (*conn).serve handles it.
					if rec == http.ErrAbortHandler {
						panic(rec)
					}
					logger.ErrorContext(r.Context(), "panic in handler",
						slog.Any("panic", rec),
						slog.String("method", r.Method),
						slog.String("path", r.URL.Path),
						slog.String("stack", string(debug.Stack())),
					)
					// If headers haven't been written yet, send a 500.
					// If they have, the connection will be broken by the client.
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusInternalServerError)
					_, _ = w.Write([]byte(`{"error":"internal server error","code":"INTERNAL_ERROR"}`))
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}
