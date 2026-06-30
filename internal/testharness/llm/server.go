package llm

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// Server hosts a cassette-backed HTTP endpoint. Clawvisor's
// CLAWVISOR_LLM_UPSTREAM_* env vars point at this URL.
//
// Cassette-key host normalization: the upstream's actual host (often a
// per-run ephemeral port) is rewritten to a stable canonical host before
// the request hits the cassette transport. That keeps the cassette key
// deterministic across runs — record once on port 12345, replay on port
// 54321, same key.
//
// Modes (driven by the underlying Cassette):
//   ModeReplay      — answers from disk; upstream URL is irrelevant
//   ModeRecord      — forwards to upstream, captures the response
//   ModePassthrough — forwards to upstream, captures nothing
type Server struct {
	srv          *httptest.Server
	cassette     *Cassette
	upstream     string // where to forward in record/passthrough modes
	canonicalURL *url.URL
}

// NewServer starts a cassette-backed HTTP server. `upstream` is the URL the
// cassette layer should forward to in record/passthrough modes (e.g., the
// real https://api.anthropic.com or a stub). For cassette key purposes the
// host is normalized to upstream's scheme+host as parsed once at construction.
func NewServer(t *testing.T, cassette *Cassette, upstream string) *Server {
	t.Helper()
	if upstream == "" {
		upstream = "https://api.anthropic.com"
	}
	canon, err := url.Parse(upstream)
	if err != nil {
		t.Fatalf("cassette: invalid upstream URL %q: %v", upstream, err)
	}
	s := &Server{cassette: cassette, upstream: upstream, canonicalURL: canon}
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
