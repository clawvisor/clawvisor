// Package expose implements `clawvisor proxy expose`: a long-running TCP
// relay that exposes the local clawvisor runtime proxy and daemon API on a
// network-routable address (e.g. for use by docker-compose isolation on a
// remote host).
//
// Two listeners — proxy + API — accept only sources matching an explicit
// allowlist of CIDRs (default: loopback + RFC-1918 + Docker default bridges)
// and relay raw TCP to the corresponding upstream. Auth is enforced upstream
// (the runtime proxy still requires a valid agent token in
// Proxy-Authorization; the API still requires its own auth headers).
package expose

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/clawvisor/clawvisor/internal/runtime/forwarder"
)

// DefaultAllowCIDRs is the conservative source-filter applied when the user
// does not pass --allow-cidr. It permits loopback (so on-host docker
// containers using `extra_hosts: host.docker.internal:host-gateway` can
// reach us) plus the three private IPv4 ranges and the Docker bridge default.
//
// IPv6 is not in the default allowlist because the v4 ranges are what 99% of
// docker-network traffic uses; users can pass `--allow-cidr ::1/128` etc. if
// they need v6.
func DefaultAllowCIDRs() []string {
	return []string{
		"127.0.0.0/8",
		"10.0.0.0/8",
		"172.16.0.0/12",
		"192.168.0.0/16",
	}
}

// Config configures Run.
type Config struct {
	// BindAddr is the host the listeners bind to (e.g. "0.0.0.0", "192.168.1.10").
	BindAddr string
	// ProxyPort is the port the proxy listener binds on (0 = random).
	ProxyPort int
	// APIPort is the port the API listener binds on (0 = random).
	APIPort int
	// ProxyUpstream is the host:port of the local runtime proxy.
	ProxyUpstream string
	// APIUpstream is the host:port of the local clawvisor daemon API.
	APIUpstream string
	// AllowCIDRs source-filter; an accepted connection's remote IP must fall
	// within at least one CIDR. Empty means use DefaultAllowCIDRs.
	AllowCIDRs []string
	// Logf, if non-nil, is invoked for one-line user-visible status messages.
	Logf func(format string, args ...any)
}

// Endpoints is the runtime view of a successfully-started Run, populated
// after the listeners bind. Useful for tests and for printing the actual
// ports back to the user when ProxyPort/APIPort were 0.
type Endpoints struct {
	ProxyAddr string
	APIAddr   string
}

// Run binds the proxy and API listeners and serves them until ctx is
// canceled or one of the underlying forwarders fails. The onReady callback,
// if non-nil, is invoked once both listeners are accepting connections; this
// is where a foreground CLI prints the bound addresses and a `--detach` flow
// can write its pidfile.
func Run(ctx context.Context, cfg Config, onReady func(Endpoints)) error {
	if err := validate(&cfg); err != nil {
		return err
	}
	allow, err := buildAllowFunc(cfg.AllowCIDRs)
	if err != nil {
		return err
	}
	logf := cfg.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}

	proxyBind := net.JoinHostPort(cfg.BindAddr, fmtPort(cfg.ProxyPort))
	apiBind := net.JoinHostPort(cfg.BindAddr, fmtPort(cfg.APIPort))

	proxyFwd, err := forwarder.Start(ctx, proxyBind, cfg.ProxyUpstream, forwarder.WithAllowFunc(allow))
	if err != nil {
		return fmt.Errorf("expose: bind proxy listener: %w", err)
	}
	apiFwd, err := forwarder.Start(ctx, apiBind, cfg.APIUpstream, forwarder.WithAllowFunc(allow))
	if err != nil {
		_ = proxyFwd.Close()
		return fmt.Errorf("expose: bind api listener: %w", err)
	}
	logf("clawvisor proxy expose: proxy %s -> %s", proxyFwd.Addr(), cfg.ProxyUpstream)
	logf("clawvisor proxy expose: api   %s -> %s", apiFwd.Addr(), cfg.APIUpstream)

	if onReady != nil {
		onReady(Endpoints{ProxyAddr: proxyFwd.Addr(), APIAddr: apiFwd.Addr()})
	}

	<-ctx.Done()
	_ = proxyFwd.Close()
	_ = apiFwd.Close()
	return nil
}

func validate(cfg *Config) error {
	if strings.TrimSpace(cfg.BindAddr) == "" {
		return errors.New("expose: BindAddr required")
	}
	if cfg.ProxyPort < 0 || cfg.ProxyPort > 65535 {
		return fmt.Errorf("expose: invalid ProxyPort %d", cfg.ProxyPort)
	}
	if cfg.APIPort < 0 || cfg.APIPort > 65535 {
		return fmt.Errorf("expose: invalid APIPort %d", cfg.APIPort)
	}
	if strings.TrimSpace(cfg.ProxyUpstream) == "" {
		return errors.New("expose: ProxyUpstream required")
	}
	if strings.TrimSpace(cfg.APIUpstream) == "" {
		return errors.New("expose: APIUpstream required")
	}
	if cfg.ProxyPort != 0 && cfg.ProxyPort == cfg.APIPort {
		return fmt.Errorf("expose: ProxyPort and APIPort must differ (both = %d)", cfg.ProxyPort)
	}
	return nil
}

// buildAllowFunc parses each CIDR and returns a predicate that returns true
// iff a TCP remote address falls within at least one parsed network. An
// empty list yields DefaultAllowCIDRs.
func buildAllowFunc(cidrs []string) (func(net.Addr) bool, error) {
	if len(cidrs) == 0 {
		cidrs = DefaultAllowCIDRs()
	}
	nets := make([]*net.IPNet, 0, len(cidrs))
	for _, c := range cidrs {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			return nil, fmt.Errorf("expose: invalid CIDR %q: %w", c, err)
		}
		nets = append(nets, n)
	}
	if len(nets) == 0 {
		return nil, errors.New("expose: at least one allow-CIDR is required")
	}
	return func(addr net.Addr) bool {
		ip := remoteIP(addr)
		if ip == nil {
			return false
		}
		for _, n := range nets {
			if n.Contains(ip) {
				return true
			}
		}
		return false
	}, nil
}

func remoteIP(addr net.Addr) net.IP {
	switch a := addr.(type) {
	case *net.TCPAddr:
		return a.IP
	case *net.UDPAddr:
		return a.IP
	default:
		host, _, err := net.SplitHostPort(addr.String())
		if err != nil {
			return nil
		}
		return net.ParseIP(host)
	}
}

func fmtPort(p int) string {
	return fmt.Sprintf("%d", p)
}
