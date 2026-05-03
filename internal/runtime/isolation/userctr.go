package isolation

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// HostKind classifies a base URL host for CLAWVISOR_URL rewrite purposes.
type HostKind int

const (
	HostKindLoopback HostKind = iota
	HostKindDNSName
	HostKindIPLiteral
)

// RewrittenURL is the result of rewriting a base URL so it points at the
// in-netns API forwarder.
type RewrittenURL struct {
	URL  string
	Kind HostKind
	// Hostname is set for HostKindDNSName: the original hostname to add via --add-host.
	Hostname string
}

// RewriteAPIURL rewrites baseURL so it routes through the API forwarder bound
// at gatewayIP:apiForwarderPort. Behavior depends on the original host:
//   - Loopback names/IPs (localhost, 127.0.0.1, ::1): host rewritten to gatewayIP.
//   - DNS hostname (e.g. clawvisor.company.internal): hostname preserved; caller
//     must add `--add-host <hostname>:<gatewayIP>` so the user container can resolve.
//   - Non-loopback IP literal (e.g. 10.0.5.50): host rewritten to gatewayIP. (Docker
//     --add-host doesn't apply to IP literals.)
//
// The scheme and path are always preserved. The port is always rewritten to
// apiForwarderPort.
func RewriteAPIURL(baseURL, gatewayIP string, apiForwarderPort int) (*RewrittenURL, error) {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return nil, fmt.Errorf("parse base URL: %w", err)
	}
	host := parsed.Hostname()
	if host == "" {
		return nil, fmt.Errorf("base URL %q has no hostname", baseURL)
	}
	rewritten := *parsed
	switch kind := classifyHost(host); kind {
	case HostKindLoopback:
		rewritten.Host = net.JoinHostPort(gatewayIP, fmt.Sprintf("%d", apiForwarderPort))
		return &RewrittenURL{URL: rewritten.String(), Kind: HostKindLoopback}, nil
	case HostKindIPLiteral:
		rewritten.Host = net.JoinHostPort(gatewayIP, fmt.Sprintf("%d", apiForwarderPort))
		return &RewrittenURL{URL: rewritten.String(), Kind: HostKindIPLiteral}, nil
	case HostKindDNSName:
		rewritten.Host = net.JoinHostPort(host, fmt.Sprintf("%d", apiForwarderPort))
		return &RewrittenURL{URL: rewritten.String(), Kind: HostKindDNSName, Hostname: host}, nil
	default:
		return nil, fmt.Errorf("unsupported host classification for %q", host)
	}
}

// ResolveUpstream returns the host:port the API forwarder should target,
// applying scheme-default ports when the URL has none.
func ResolveUpstream(baseURL string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return "", fmt.Errorf("parse base URL: %w", err)
	}
	host := parsed.Hostname()
	if host == "" {
		return "", fmt.Errorf("base URL %q has no hostname", baseURL)
	}
	port := parsed.Port()
	if port == "" {
		switch parsed.Scheme {
		case "https":
			port = "443"
		default:
			port = "80"
		}
	}
	return net.JoinHostPort(host, port), nil
}

func classifyHost(host string) HostKind {
	host = strings.TrimSpace(host)
	switch strings.ToLower(host) {
	case "localhost", "127.0.0.1", "::1":
		return HostKindLoopback
	}
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsLoopback() {
			return HostKindLoopback
		}
		return HostKindIPLiteral
	}
	return HostKindDNSName
}

// ConflictingFlags is the set of `docker run` flags that conflict with
// isolation (we own the network and DNS for the user container).
var ConflictingFlags = []string{
	"--network", "--net", "--add-host", "--dns", "--dns-search",
}

// CheckUserArgs scans argv for flags that conflict with isolation and returns
// an error describing the first conflict found.
func CheckUserArgs(args []string) error {
	for _, tok := range args {
		base := tok
		if i := strings.IndexByte(tok, '='); i >= 0 {
			base = tok[:i]
		}
		for _, bad := range ConflictingFlags {
			if base == bad {
				return fmt.Errorf("--isolation=container manages networking; remove %s from your docker run args", bad)
			}
		}
	}
	return nil
}
