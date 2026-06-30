package llm

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Server hosts a cassette-backed HTTP endpoint. Clawvisor's
// CLAWVISOR_LLM_UPSTREAM_* env vars point at this URL.
//
// Cassette-key stability across runs is achieved by Cassette.requestKey
// omitting the host entirely — so this server does not need to perform
// any URL rewriting before invoking the cassette transport.
//
// Modes (driven by the underlying Cassette):
//   ModeReplay      — answers from disk; upstream URL is irrelevant
//   ModeRecord      — forwards to upstream, captures the response
//   ModePassthrough — forwards to upstream, captures nothing
type Server struct {
	srv      *httptest.Server
	cassette *Cassette
	upstream string // where to forward in record/passthrough modes
}

// NewServer starts a cassette-backed HTTP server. `upstream` is the URL
// the cassette layer should forward to in record/passthrough modes
// (e.g., the real https://api.anthropic.com or a stub). In replay mode
// the upstream URL is not consulted.
func NewServer(t *testing.T, cassette *Cassette, upstream string) *Server {
	t.Helper()
	if upstream == "" {
		upstream = "https://api.anthropic.com"
	}
	s := &Server{cassette: cassette, upstream: upstream}
	s.srv = httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *Server) URL() string { return s.srv.URL }

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	upstreamReq, err := http.NewRequestWithContext(r.Context(), r.Method,
		s.upstream+r.URL.RequestURI(), r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	for k, v := range r.Header {
		switch k {
		case "Host", "Connection", "Content-Length":
			continue
		}
		upstreamReq.Header[k] = v
	}

	resp, err := s.cassette.RoundTrip(upstreamReq)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	for k, v := range resp.Header {
		w.Header()[k] = v
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}
