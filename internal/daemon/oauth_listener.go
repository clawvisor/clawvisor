package daemon

import (
	"context"
	"net"
	"net/http"
	"sync"
)

// startOAuthListener starts a one-shot HTTP server on a random localhost port.
// It returns the port, a channel that closes when the /oauth-done callback
// fires, and a cleanup function that shuts down the server.
//
// Copied from internal/tui/screens/services.go — kept in sync manually.
func startOAuthListener() (port int, done chan struct{}, cleanup func()) {
	done = make(chan struct{})
	var once sync.Once

	mux := http.NewServeMux()
	mux.HandleFunc("/oauth-done", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
		once.Do(func() { close(done) })
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		// Fallback: return a closed channel so the caller doesn't block forever.
		close(done)
		return 0, done, func() {}
	}

	port = ln.Addr().(*net.TCPAddr).Port
	srv := &http.Server{Handler: mux}

	go func() { _ = srv.Serve(ln) }()

	cleanup = func() {
		once.Do(func() { close(done) })
		_ = srv.Shutdown(context.Background())
	}
	return port, done, cleanup
}
