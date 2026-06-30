// Package httpmock is a small scriptable HTTP server used by per-service
// mocks (Slack, GitHub, Notion, Linear, Twilio, Telegram, Microsoft Graph,
// Stripe). Each mock declares a route + a default response; tests override
// via `mock.OnNext(...)` for one-shot scripts or `mock.OnEvery(...)` for
// sticky responses.
package httpmock

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
)

// Response is what handler scripts return.
type Response struct {
	Status int
	Body   any               // marshalled to JSON if not []byte / string / nil
	Header map[string]string // optional response headers
}

// Captured is one captured request (method, path, body, headers).
type Captured struct {
	Method  string
	Path    string
	Query   string
	Body    []byte
	Headers http.Header
}

// Server wraps an httptest.Server with scripting + capture.
type Server struct {
	mu       sync.Mutex
	srv      *httptest.Server
	routes   map[string]*routeState // key = "METHOD PATH"
	captured []Captured
}

type routeState struct {
	defaultResp Response
	nextResps   []Response // FIFO; consumed per request
	everyResp   *Response  // sticky
}

// New starts a fresh server with the given default routes (each entry is
// "METHOD /path" → default Response). Cleanup runs via t.Cleanup.
func New(t *testing.T, defaults map[string]Response) *Server {
	t.Helper()
	s := &Server{routes: map[string]*routeState{}}
	mux := http.NewServeMux()
	// Register routes from defaults.
	keys := make([]string, 0, len(defaults))
	for k := range defaults {
		keys = append(keys, k)
	}
	sort.Strings(keys) // stable order (mux.Handle panics on duplicate)
	for _, k := range keys {
		s.routes[k] = &routeState{defaultResp: defaults[k]}
		// Mux uses Go 1.22+ method-path patterns.
		mux.HandleFunc(k, s.handle(k))
	}
	s.srv = httptest.NewServer(mux)
	t.Cleanup(s.srv.Close)
	return s
}

// URL returns the server's base URL.
func (s *Server) URL() string { return s.srv.URL }

// OnNext queues a one-shot response for the next request matching key.
// Calls are FIFO — three OnNext calls give responses to the next three
// requests in order, then it falls back to the default.
func (s *Server) OnNext(key string, r Response) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rs, ok := s.routes[key]
	if !ok {
		panic("httpmock: OnNext unknown route " + key)
	}
	rs.nextResps = append(rs.nextResps, r)
}

// OnEvery sets a sticky response for all subsequent matches. Clears OnNext.
func (s *Server) OnEvery(key string, r Response) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rs, ok := s.routes[key]
	if !ok {
		panic("httpmock: OnEvery unknown route " + key)
	}
	rs.everyResp = &r
	rs.nextResps = nil
}

// Captured returns all captured requests in order.
func (s *Server) Captured() []Captured {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Captured, len(s.captured))
	copy(out, s.captured)
	return out
}

// AssertReceived fails the test if no captured request matches the given
// method+path (path may be a substring). Returns the first matching request.
func (s *Server) AssertReceived(t *testing.T, method, pathSubstr string) Captured {
	t.Helper()
	for _, c := range s.Captured() {
		if c.Method == method && strings.Contains(c.Path, pathSubstr) {
			return c
		}
	}
	t.Fatalf("httpmock: no %s request matching %q (captured %d)", method, pathSubstr, len(s.Captured()))
	return Captured{}
}

// Reset clears captured + scripted state.
func (s *Server) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.captured = nil
	for _, rs := range s.routes {
		rs.nextResps = nil
		rs.everyResp = nil
	}
}

func (s *Server) handle(key string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body.Close()

		s.mu.Lock()
		s.captured = append(s.captured, Captured{
			Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery,
			Body: body, Headers: r.Header.Clone(),
		})
		rs := s.routes[key]
		var resp Response
		switch {
		case len(rs.nextResps) > 0:
			resp = rs.nextResps[0]
			rs.nextResps = rs.nextResps[1:]
		case rs.everyResp != nil:
			resp = *rs.everyResp
		default:
			resp = rs.defaultResp
		}
		s.mu.Unlock()

		for k, v := range resp.Header {
			w.Header().Set(k, v)
		}
		if resp.Status == 0 {
			resp.Status = http.StatusOK
		}
		// Default content-type for JSON-ish responses.
		if w.Header().Get("Content-Type") == "" && resp.Body != nil {
			w.Header().Set("Content-Type", "application/json")
		}
		w.WriteHeader(resp.Status)

		switch b := resp.Body.(type) {
		case nil:
			// no body
		case []byte:
			_, _ = w.Write(b)
		case string:
			_, _ = w.Write([]byte(b))
		default:
			buf := &bytes.Buffer{}
			_ = json.NewEncoder(buf).Encode(b)
			_, _ = w.Write(buf.Bytes())
		}
	}
}
