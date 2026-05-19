package llmproxy

import (
	"context"
	"net"
	"net/url"
	"strings"

	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/inspector"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// RuntimePlaceholderBoundHosts returns the authoritative host allowlist for a
// runtime placeholder. Known services use the built-in service boundary; custom
// services may only fall back to the original credential authorization host.
func RuntimePlaceholderBoundHosts(ctx context.Context, st store.Store, ph *store.RuntimePlaceholder) ([]string, string) {
	if ph == nil {
		return nil, "placeholder missing"
	}
	hosts := inspector.BoundServiceHosts(ph.ServiceID)
	if len(hosts) > 0 {
		return hosts, ""
	}
	serviceID := strings.TrimSpace(ph.ServiceID)
	if ph.CredentialGrantID == "" {
		return nil, "no bound-service hosts for service " + serviceID
	}
	if st == nil {
		return nil, "no store configured for credential grant host lookup"
	}
	auth, err := st.GetCredentialAuthorization(ctx, ph.CredentialGrantID)
	if err != nil {
		return nil, "credential grant host lookup failed"
	}
	host := normalizeCredentialAuthorizationHost(auth.Host)
	if host == "" {
		return nil, "no bound-service hosts for service " + serviceID
	}
	return []string{host}, ""
}

func normalizeCredentialAuthorizationHost(host string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" {
		return ""
	}
	if parsed, err := url.Parse(host); err == nil && parsed.Host != "" {
		host = parsed.Host
	}
	if hostOnly, _, err := net.SplitHostPort(host); err == nil {
		host = hostOnly
	}
	return strings.TrimSuffix(host, ".")
}
