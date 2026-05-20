package yamlruntime

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"
)

var allowPrivateNetworkTargetsForTests bool

var yamlRuntimeSSRFRanges = func() []*net.IPNet {
	cidrs := []string{
		"0.0.0.0/8",
		"10.0.0.0/8",
		"127.0.0.0/8",
		"169.254.0.0/16",
		"172.16.0.0/12",
		"192.168.0.0/16",
		"::1/128",
		"fc00::/7",
		"fe80::/10",
	}
	nets := make([]*net.IPNet, 0, len(cidrs))
	for _, cidr := range cidrs {
		_, n, _ := net.ParseCIDR(cidr)
		nets = append(nets, n)
	}
	return nets
}()

func yamlRuntimeHTTPClient(base http.RoundTripper) *http.Client {
	return &http.Client{
		Timeout:   30 * time.Second,
		Transport: yamlRuntimeSSRFTransport(base),
	}
}

func yamlRuntimeHTTPClientWithTransport(transport http.RoundTripper) *http.Client {
	return &http.Client{
		Timeout:   30 * time.Second,
		Transport: transport,
	}
}

func yamlRuntimeSSRFTransport(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	transport, ok := base.(*http.Transport)
	if !ok {
		return &ssrfCheckingRoundTripper{base: base}
	}
	clone := transport.Clone()
	clone.Proxy = nil
	clone.DialContext = yamlRuntimeSSRFSafeDialContext
	return &ssrfCheckingRoundTripper{base: clone}
}

func yamlRuntimeSSRFSafeDialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("yamlruntime: invalid address %q: %w", address, err)
	}

	ips, err := net.DefaultResolver.LookupHost(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("yamlruntime: cannot resolve host %q: %w", host, err)
	}
	for _, ipStr := range ips {
		ip := net.ParseIP(ipStr)
		if ip == nil {
			continue
		}
		if yamlRuntimeIsSSRFTarget(ip) {
			return nil, fmt.Errorf("yamlruntime: host %q resolves to blocked IP %s", host, ipStr)
		}
		return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, net.JoinHostPort(ipStr, port))
	}
	return nil, fmt.Errorf("yamlruntime: no safe IPs found for host %q", host)
}

func yamlRuntimeIsSSRFTarget(ip net.IP) bool {
	if allowPrivateNetworkTargetsForTests || os.Getenv("CLAWVISOR_YAMLRUNTIME_ALLOW_PRIVATE_NETWORKS") == "1" {
		return false
	}
	for _, n := range yamlRuntimeSSRFRanges {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

type ssrfCheckingRoundTripper struct {
	base http.RoundTripper
}

func (t *ssrfCheckingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	host := req.URL.Hostname()
	if host == "" {
		return nil, fmt.Errorf("yamlruntime: request URL must include a host")
	}
	ips, err := net.DefaultResolver.LookupHost(req.Context(), host)
	if err != nil {
		return nil, fmt.Errorf("yamlruntime: cannot resolve host %q: %w", host, err)
	}
	for _, ipStr := range ips {
		ip := net.ParseIP(ipStr)
		if ip != nil && yamlRuntimeIsSSRFTarget(ip) {
			return nil, fmt.Errorf("yamlruntime: host %q resolves to blocked IP %s", host, ipStr)
		}
	}
	return t.base.RoundTrip(req)
}
