package harness

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"

	runtimeproxy "github.com/clawvisor/clawvisor/pkg/runtime/proxy"
)

// Upstreams installs in-process httptest mocks behind hostnames and rewires
// the proxy's outbound transport so traffic the agent dials at e.g.
// "api.gmail.com" hits the local mock instead of the real internet.
//
// The agent talks to the proxy at proxy_url; the proxy then connects out via
// its goproxy.Tr.DialContext to the host the agent named. We override that
// DialContext to a fake-resolver that maps a configured host to the mock
// server's address.
type Upstreams struct {
	proxy *runtimeproxy.Server

	mu     sync.Mutex
	hosts  map[string]*httptest.Server
	hits   map[string]int
	closed bool
}

// NewUpstreams installs the dial override on proxy and returns a manager
// that scenarios add fake upstreams to.
func NewUpstreams(proxy *runtimeproxy.Server) (*Upstreams, error) {
	if proxy == nil {
		return nil, fmt.Errorf("upstreams: proxy is required")
	}
	u := &Upstreams{
		proxy: proxy,
		hosts: make(map[string]*httptest.Server),
		hits:  make(map[string]int),
	}
	tr := proxy.GoProxy().Tr
	tr.DialContext = u.dial
	// httptest TLS servers self-sign for "example.com". Scenarios will hit
	// arbitrary virtual hosts (api.gmail.com, slack.com, …), so verify is off
	// inside the harness.
	tr.TLSClientConfig = &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: true,
	}
	return u, nil
}

// AddJSON registers a host that returns the given fixed status+JSON body for
// any request. Returns the *httptest.Server so the caller can layer extra
// behavior if needed.
func (u *Upstreams) AddJSON(host string, status int, jsonBody string) *httptest.Server {
	return u.AddHandler(host, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(jsonBody))
	}))
}

// AddHandler registers an arbitrary handler at host. The httptest server is
// started in TLS mode so https://host requests work. Every request through
// this host increments the per-host hit counter exposed via Hits.
func (u *Upstreams) AddHandler(host string, h http.Handler) *httptest.Server {
	u.mu.Lock()
	defer u.mu.Unlock()
	if existing, ok := u.hosts[host]; ok {
		existing.Close()
	}
	wrapped := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		u.hits[host]++
		u.mu.Unlock()
		h.ServeHTTP(w, r)
	})
	srv := httptest.NewTLSServer(wrapped)
	u.hosts[host] = srv
	return srv
}

// Hits returns how many requests the given host has handled.
func (u *Upstreams) Hits(host string) int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.hits[host]
}

// Hosts returns the registered virtual hostnames in insertion-undefined
// order. Used by the harness to advertise the upstream surface to the agent.
func (u *Upstreams) Hosts() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := make([]string, 0, len(u.hosts))
	for host := range u.hosts {
		out = append(out, host)
	}
	return out
}

// Close shuts every mock server down. Safe to call multiple times.
func (u *Upstreams) Close() {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.closed {
		return
	}
	for _, srv := range u.hosts {
		srv.Close()
	}
	u.hosts = nil
	u.closed = true
}

func (u *Upstreams) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	u.mu.Lock()
	srv, ok := u.hosts[strings.ToLower(host)]
	u.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("upstreams: unregistered host %q (registered: %v)", host, u.Hosts())
	}
	target, err := url.Parse(srv.URL)
	if err != nil {
		return nil, fmt.Errorf("upstreams: parse mock URL %q: %w", srv.URL, err)
	}
	var d net.Dialer
	return d.DialContext(ctx, network, target.Host)
}
