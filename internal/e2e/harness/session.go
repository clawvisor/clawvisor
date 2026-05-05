package harness

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"net/url"

	runtimeproxy "github.com/clawvisor/clawvisor/internal/runtime/proxy"
)

// SessionHandle bundles everything the responder needs to talk through the
// proxy under test: the session id (so other tests can scope queries), the
// proxy URL, the bearer secret, and a *http.Client wired to use them.
type SessionHandle struct {
	SessionID   string
	ProxyURL    string
	ProxyBearer string
	Client      *http.Client
}

// CreateSession mints a runtime session for the given principal, then
// returns a handle whose http.Client routes through the proxy with the
// session's Proxy-Authorization header pre-set.
func (s *Server) CreateSession(ctx context.Context, p *Principal) (*SessionHandle, error) {
	if s == nil {
		return nil, fmt.Errorf("harness: server not started")
	}
	if p == nil || p.Agent == nil || p.User == nil {
		return nil, fmt.Errorf("harness: principal is required")
	}
	res, err := s.Manager.CreateRuntimeSession(ctx, p.Agent.ID, p.User.ID, runtimeproxy.CreateSessionRequest{
		Mode: "proxy",
	})
	if err != nil {
		return nil, fmt.Errorf("harness: create runtime session: %w", err)
	}
	p.Session = res.Session

	proxyURL, err := url.Parse(res.ProxyURL)
	if err != nil {
		return nil, fmt.Errorf("harness: parse proxy url %q: %w", res.ProxyURL, err)
	}
	caPool := x509.NewCertPool()
	if ca := s.Proxy.CA(); ca != nil {
		caPool.AddCert(ca)
	}
	connectHeader := http.Header{}
	connectHeader.Set("Proxy-Authorization", "Bearer "+res.ProxyBearer)
	client := &http.Client{
		Transport: &bearerTransport{
			bearer: res.ProxyBearer,
			base: &http.Transport{
				Proxy: http.ProxyURL(proxyURL),
				TLSClientConfig: &tls.Config{
					RootCAs:    caPool,
					MinVersion: tls.VersionTLS12,
				},
				ProxyConnectHeader: connectHeader,
			},
		},
	}
	// Wire the test.echo adapter's fetch_url action through the same
	// session-scoped client. Adapter-issued requests then pass through
	// the runtime proxy and the upstreams DialContext just like the
	// agent's direct calls — same policy, same per-host hit counters.
	if s.API != nil && s.API.Echo != nil {
		s.API.Echo.SetFetchClient(client)
	}

	return &SessionHandle{
		SessionID:   res.Session.ID,
		ProxyURL:    res.ProxyURL,
		ProxyBearer: res.ProxyBearer,
		Client:      client,
	}, nil
}

type bearerTransport struct {
	bearer string
	base   *http.Transport
}

func (t *bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.Header.Set("Proxy-Authorization", "Bearer "+t.bearer)
	return t.base.RoundTrip(clone)
}
