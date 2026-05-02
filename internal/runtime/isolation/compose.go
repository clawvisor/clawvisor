package isolation

import (
	"fmt"
	"io"
	"net"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// HolderServiceName is the compose service name for the netns-holder sidecar
// emitted by ComposeIsolationOverride.
const HolderServiceName = "clawvisor-netns-holder"

// ComposeExposeEndpoints describes the standalone `clawvisor proxy expose`
// process the holder + user service should route through.
type ComposeExposeEndpoints struct {
	// ProxyURL is the full URL of the expose proxy listener
	// (e.g. "http://192.168.1.10:25291"). Scheme + host + port required.
	ProxyURL string
	// APIURL is the full URL of the expose API listener
	// (e.g. "https://clawvisor.company.internal:18791"). Scheme + host + port required.
	APIURL string
}

// ComposeIsolationPlan is the input to ComposeIsolationOverride.
type ComposeIsolationPlan struct {
	// UserService is the compose service name to wire through the holder.
	UserService string
	// HolderImage is the resolved isolation image tag (e.g. "clawvisor-isolation:abc123").
	HolderImage string
	// Expose carries the parsed expose URLs.
	Expose ComposeExposeEndpoints
	// EnvVars are the env vars to set on the user service (proxy URLs, CLAWVISOR_URL, CA paths…).
	EnvVars []ComposeEnvVar
	// CAHostPath is the path to the runtime proxy CA on the host.
	CAHostPath string
	// CAContainerPath is the mount path inside the user container.
	CAContainerPath string
}

// ComposeEnvVar is a single environment variable destined for the user service.
type ComposeEnvVar struct {
	Key     string
	Value   string
	Comment string
}

// ParsedExpose is the result of parsing a single expose URL.
type ParsedExpose struct {
	Scheme string
	Host   string
	Port   int
	// HostKind classifies Host (loopback / DNS / IP).
	HostKind HostKind
}

// ParseExposeURL parses an expose URL and validates its shape: must have a
// scheme (http or https), a non-empty host, and an explicit port (default
// derived from scheme if missing). Loopback hosts are rejected because the
// expose process is by definition reachable from another container.
func ParseExposeURL(rawURL, label string) (ParsedExpose, error) {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return ParsedExpose{}, fmt.Errorf("%s: parse %q: %w", label, rawURL, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return ParsedExpose{}, fmt.Errorf("%s: %q has unsupported scheme (want http or https)", label, rawURL)
	}
	host := parsed.Hostname()
	if host == "" {
		return ParsedExpose{}, fmt.Errorf("%s: %q has no hostname", label, rawURL)
	}
	kind := classifyHost(host)
	if kind == HostKindLoopback {
		return ParsedExpose{}, fmt.Errorf("%s: %q points at loopback; expose URLs must be reachable from the holder", label, rawURL)
	}
	// init-firewall.sh is IPv4-only (resolves via getent ahostsv4 and the
	// iptables ACCEPT rules use IPv4 destinations). Reject IPv6 literals up
	// front so the holder doesn't fail at startup with a confusing message.
	if ip := net.ParseIP(host); ip != nil && ip.To4() == nil {
		return ParsedExpose{}, fmt.Errorf("%s: %q is an IPv6 literal; only IPv4 hosts are supported", label, rawURL)
	}
	portStr := parsed.Port()
	if portStr == "" {
		switch parsed.Scheme {
		case "https":
			portStr = "443"
		default:
			portStr = "80"
		}
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 || port > 65535 {
		return ParsedExpose{}, fmt.Errorf("%s: %q has invalid port", label, rawURL)
	}
	return ParsedExpose{Scheme: parsed.Scheme, Host: host, Port: port, HostKind: kind}, nil
}

// HostPort returns the host:port string for use in URL construction.
func (p ParsedExpose) HostPort() string {
	return net.JoinHostPort(p.Host, strconv.Itoa(p.Port))
}

// EmitComposeIsolationOverride writes a docker-compose override that adds the
// netns-holder sidecar and rewires the user service to share its netns. The
// holder + both expose URLs together guarantee the user service can only egress
// to the two expose listeners.
//
// The holder + user service must agree on the expose host's IP for iptables
// matching to work. When the host is a DNS name we rely on consistent DNS
// resolution inside the docker network (or operator-managed extra_hosts).
// Compose forbids `extra_hosts` on a service with `network_mode: service:…`,
// so we don't emit one on the user service; if you need a fixed mapping for a
// hostname-based expose URL, configure the docker network DNS or pin the
// expose URL to an IP literal.
func EmitComposeIsolationOverride(w io.Writer, plan ComposeIsolationPlan) error {
	if plan.UserService == "" {
		return fmt.Errorf("compose isolation: UserService required")
	}
	if plan.HolderImage == "" {
		return fmt.Errorf("compose isolation: HolderImage required")
	}
	proxyExpose, err := ParseExposeURL(plan.Expose.ProxyURL, "--expose-url")
	if err != nil {
		return err
	}
	apiExpose, err := ParseExposeURL(plan.Expose.APIURL, "--expose-api-url")
	if err != nil {
		return err
	}
	if proxyExpose.Host != apiExpose.Host {
		return fmt.Errorf("compose isolation: --expose-url host (%q) and --expose-api-url host (%q) must be the same so the holder iptables rules apply consistently",
			proxyExpose.Host, apiExpose.Host)
	}

	fmt.Fprintf(w, "# clawvisor agent docker-compose --isolation=container override for service=%q\n", plan.UserService)
	fmt.Fprintln(w, "#")
	fmt.Fprintln(w, "# This override adds a privileged netns-holder sidecar that installs an")
	fmt.Fprintln(w, "# iptables-locked egress policy: the user service can only reach the two")
	fmt.Fprintln(w, "# `clawvisor proxy expose` listeners — TCP to any other destination is")
	fmt.Fprintln(w, "# rejected by the kernel regardless of HTTPS_PROXY honoring in userspace.")
	fmt.Fprintln(w, "#")
	fmt.Fprintf(w, "# Egress is locked to: %s (proxy %d, api %d)\n", proxyExpose.Host, proxyExpose.Port, apiExpose.Port)
	fmt.Fprintln(w, "#")
	fmt.Fprintln(w, "# Compose forbids extra_hosts on a service using `network_mode: service:…`.")
	fmt.Fprintln(w, "# If your expose URL uses a hostname, ensure the docker network's DNS")
	fmt.Fprintln(w, "# resolves it consistently for both the holder and user containers.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "services:")

	emitHolderService(w, plan.HolderImage, proxyExpose, apiExpose)
	fmt.Fprintln(w)
	emitUserService(w, plan)
	return nil
}

func emitHolderService(w io.Writer, image string, proxy, api ParsedExpose) {
	fmt.Fprintf(w, "  %s:\n", HolderServiceName)
	fmt.Fprintf(w, "    image: %s\n", yamlQuote(image))
	fmt.Fprintln(w, "    cap_add:")
	fmt.Fprintln(w, "      - NET_ADMIN")
	fmt.Fprintln(w, "      - NET_RAW")
	fmt.Fprintln(w, "    restart: unless-stopped")
	fmt.Fprintln(w, "    environment:")
	fmt.Fprintf(w, "      CLAWVISOR_HOST_TARGET: %s\n", yamlQuote(proxy.Host))
	fmt.Fprintf(w, "      CLAWVISOR_PROXY_PORT: %s\n", yamlQuote(strconv.Itoa(proxy.Port)))
	fmt.Fprintf(w, "      CLAWVISOR_API_PORT: %s\n", yamlQuote(strconv.Itoa(api.Port)))
	fmt.Fprintln(w, "    healthcheck:")
	fmt.Fprintln(w, `      test: ["CMD", "test", "-f", "/run/clawvisor/firewall.ready"]`)
	fmt.Fprintln(w, "      interval: 1s")
	fmt.Fprintln(w, "      retries: 30")
	fmt.Fprintln(w, "      start_period: 2s")
}

func emitUserService(w io.Writer, plan ComposeIsolationPlan) {
	fmt.Fprintf(w, "  %s:\n", plan.UserService)
	fmt.Fprintf(w, "    network_mode: %s\n", yamlQuote("service:"+HolderServiceName))
	fmt.Fprintln(w, "    depends_on:")
	fmt.Fprintf(w, "      %s:\n", HolderServiceName)
	fmt.Fprintln(w, "        condition: service_healthy")
	fmt.Fprintln(w, "    environment:")
	keyed := make(map[string]ComposeEnvVar, len(plan.EnvVars))
	keys := make([]string, 0, len(plan.EnvVars))
	for _, v := range plan.EnvVars {
		keyed[v.Key] = v
		keys = append(keys, v.Key)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := keyed[k]
		if v.Comment != "" {
			fmt.Fprintf(w, "      # %s\n", v.Comment)
		}
		fmt.Fprintf(w, "      %s: %s\n", v.Key, yamlQuote(v.Value))
	}
	if plan.CAHostPath != "" && plan.CAContainerPath != "" {
		fmt.Fprintln(w, "    volumes:")
		fmt.Fprintf(w, "      - %s\n", yamlQuote(fmt.Sprintf("%s:%s:ro", plan.CAHostPath, plan.CAContainerPath)))
	}
}

func yamlQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
