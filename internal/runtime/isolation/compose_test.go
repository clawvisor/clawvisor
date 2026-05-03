package isolation

import (
	"bytes"
	"strings"
	"testing"
)

func TestParseExposeURL(t *testing.T) {
	cases := []struct {
		name     string
		raw      string
		wantHost string
		wantPort int
		wantKind HostKind
	}{
		{"ip literal http", "http://192.168.1.10:25291", "192.168.1.10", 25291, HostKindIPLiteral},
		{"hostname https", "https://clawvisor.company.internal:18791", "clawvisor.company.internal", 18791, HostKindDNSName},
		{"hostname https default port", "https://clawvisor.company.internal", "clawvisor.company.internal", 443, HostKindDNSName},
		{"hostname http default port", "http://clawvisor.company.internal", "clawvisor.company.internal", 80, HostKindDNSName},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ParseExposeURL(c.raw, "test")
			if err != nil {
				t.Fatalf("ParseExposeURL: %v", err)
			}
			if got.Host != c.wantHost || got.Port != c.wantPort || got.HostKind != c.wantKind {
				t.Errorf("got %+v want host=%q port=%d kind=%d", got, c.wantHost, c.wantPort, c.wantKind)
			}
		})
	}
}

func TestParseExposeURLRejects(t *testing.T) {
	cases := map[string]string{
		"loopback ip":   "http://127.0.0.1:25291",
		"loopback name": "http://localhost:25291",
		"empty scheme":  "192.168.1.10:25291",
		"bad scheme":    "ftp://192.168.1.10:25291",
		"no host":       "http://:25291",
		"ipv6 literal":  "http://[2001:db8::1]:25291",
		"ipv6 loopback": "http://[::1]:25291",
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseExposeURL(raw, "test"); err == nil {
				t.Fatalf("expected error for %s (%s)", name, raw)
			}
		})
	}
}

func TestEmitComposeIsolationOverrideRejectsMismatchedHosts(t *testing.T) {
	plan := ComposeIsolationPlan{
		UserService: "agent",
		HolderImage: "clawvisor-isolation:abc123",
		Expose: ComposeExposeEndpoints{
			ProxyURL: "http://192.168.1.10:25291",
			APIURL:   "https://192.168.1.99:18791",
		},
	}
	var buf bytes.Buffer
	if err := EmitComposeIsolationOverride(&buf, plan); err == nil {
		t.Fatal("expected error for mismatched expose hosts")
	}
}

func TestEmitComposeIsolationOverrideRequiresUserService(t *testing.T) {
	var buf bytes.Buffer
	err := EmitComposeIsolationOverride(&buf, ComposeIsolationPlan{
		HolderImage: "clawvisor-isolation:x",
		Expose:      ComposeExposeEndpoints{ProxyURL: "http://192.168.1.10:25291", APIURL: "http://192.168.1.10:18791"},
	})
	if err == nil {
		t.Fatal("expected error when UserService is empty")
	}
}

func TestEmitComposeIsolationOverrideIPLiteralGolden(t *testing.T) {
	plan := ComposeIsolationPlan{
		UserService: "agent",
		HolderImage: "clawvisor-isolation:abc123",
		Expose: ComposeExposeEndpoints{
			ProxyURL: "http://192.168.1.10:25291",
			APIURL:   "http://192.168.1.10:18791",
		},
		EnvVars: []ComposeEnvVar{
			{Key: "CLAWVISOR_URL", Value: "http://192.168.1.10:18791", Comment: "Clawvisor API URL"},
			{Key: "HTTPS_PROXY", Value: "http://launch-x:tok@192.168.1.10:25291", Comment: ""},
		},
		CAHostPath:      "/Users/op/.clawvisor/runtime-proxy/ca.pem",
		CAContainerPath: "/clawvisor/ca.pem",
	}
	var buf bytes.Buffer
	if err := EmitComposeIsolationOverride(&buf, plan); err != nil {
		t.Fatalf("emit: %v", err)
	}
	out := buf.String()

	wantSubstrings := []string{
		"services:",
		"  clawvisor-netns-holder:",
		`    image: "clawvisor-isolation:abc123"`,
		"      - NET_ADMIN",
		"      - NET_RAW",
		`      CLAWVISOR_HOST_TARGET: "192.168.1.10"`,
		`      CLAWVISOR_PROXY_PORT: "25291"`,
		`      CLAWVISOR_API_PORT: "18791"`,
		"    healthcheck:",
		`      test: ["CMD", "test", "-f", "/run/clawvisor/firewall.ready"]`,
		"  agent:",
		`    network_mode: "service:clawvisor-netns-holder"`,
		"    depends_on:",
		"        condition: service_healthy",
		`      CLAWVISOR_URL: "http://192.168.1.10:18791"`,
		`      HTTPS_PROXY: "http://launch-x:tok@192.168.1.10:25291"`,
		`      - "/Users/op/.clawvisor/runtime-proxy/ca.pem:/clawvisor/ca.pem:ro"`,
	}
	for _, s := range wantSubstrings {
		if !strings.Contains(out, s) {
			t.Errorf("output missing %q\n--- output ---\n%s", s, out)
		}
	}
}

func TestEmitComposeIsolationOverridePublishPorts(t *testing.T) {
	plan := ComposeIsolationPlan{
		UserService: "agent",
		HolderImage: "clawvisor-isolation:abc123",
		Expose: ComposeExposeEndpoints{
			ProxyURL: "http://192.168.1.10:25291",
			APIURL:   "http://192.168.1.10:18791",
		},
		EnvVars:      []ComposeEnvVar{{Key: "CLAWVISOR_URL", Value: "http://192.168.1.10:18791"}},
		PublishPorts: []string{"18789:18789", "0.0.0.0:18790:18790/tcp"},
	}
	var buf bytes.Buffer
	if err := EmitComposeIsolationOverride(&buf, plan); err != nil {
		t.Fatalf("emit: %v", err)
	}
	out := buf.String()

	// Holder must publish; user service must reset its inherited ports list.
	holderBlock, userBlock := splitServices(t, out, "agent")
	if !strings.Contains(holderBlock, "ports:") {
		t.Errorf("holder service should emit ports:\n%s", holderBlock)
	}
	if !strings.Contains(holderBlock, `      - "18789:18789"`) {
		t.Errorf("holder missing 18789 port:\n%s", holderBlock)
	}
	if !strings.Contains(holderBlock, `      - "0.0.0.0:18790:18790/tcp"`) {
		t.Errorf("holder missing tcp port:\n%s", holderBlock)
	}
	if !strings.Contains(userBlock, "ports: !reset []") {
		t.Errorf("user service must clear inherited ports with !reset []:\n%s", userBlock)
	}
}

func TestEmitComposeIsolationOverrideNoPublishPortsByDefault(t *testing.T) {
	plan := ComposeIsolationPlan{
		UserService: "agent",
		HolderImage: "clawvisor-isolation:abc123",
		Expose: ComposeExposeEndpoints{
			ProxyURL: "http://192.168.1.10:25291",
			APIURL:   "http://192.168.1.10:18791",
		},
		EnvVars: []ComposeEnvVar{{Key: "CLAWVISOR_URL", Value: "http://192.168.1.10:18791"}},
	}
	var buf bytes.Buffer
	if err := EmitComposeIsolationOverride(&buf, plan); err != nil {
		t.Fatalf("emit: %v", err)
	}
	out := buf.String()
	yamlOnly := strings.SplitN(out, "services:\n", 2)[1]
	if strings.Contains(yamlOnly, "ports:") {
		t.Errorf("no PublishPorts → no ports keys should appear:\n%s", out)
	}
}

func TestEmitComposeIsolationOverrideRejectsBadPublishPorts(t *testing.T) {
	cases := map[string][]string{
		"empty":      {""},
		"whitespace": {" 18789:18789"},
		"newline":    {"18789:18789\n"},
		"quote":      {`18789:18789"`},
	}
	for name, ports := range cases {
		t.Run(name, func(t *testing.T) {
			plan := ComposeIsolationPlan{
				UserService: "agent",
				HolderImage: "clawvisor-isolation:abc123",
				Expose: ComposeExposeEndpoints{
					ProxyURL: "http://192.168.1.10:25291",
					APIURL:   "http://192.168.1.10:18791",
				},
				PublishPorts: ports,
			}
			var buf bytes.Buffer
			if err := EmitComposeIsolationOverride(&buf, plan); err == nil {
				t.Fatalf("expected error for %s (%q)", name, ports)
			}
		})
	}
}

// splitServices returns the holder block and the named user-service block
// from an emitted override. Used so port-related assertions can target the
// correct block independent of emission order.
func splitServices(t *testing.T, out, userSvc string) (holder, user string) {
	t.Helper()
	holderTag := "  " + HolderServiceName + ":"
	userTag := "  " + userSvc + ":"
	hi := strings.Index(out, holderTag)
	ui := strings.Index(out, userTag)
	if hi < 0 || ui < 0 {
		t.Fatalf("could not find both service blocks in output:\n%s", out)
	}
	if hi < ui {
		return out[hi:ui], out[ui:]
	}
	return out[hi:], out[ui:hi]
}

func TestEmitComposeIsolationOverrideHostnameGolden(t *testing.T) {
	plan := ComposeIsolationPlan{
		UserService: "agent",
		HolderImage: "clawvisor-isolation:abc123",
		Expose: ComposeExposeEndpoints{
			ProxyURL: "https://clawvisor.company.internal:25291",
			APIURL:   "https://clawvisor.company.internal:18791",
		},
		EnvVars: []ComposeEnvVar{
			{Key: "CLAWVISOR_URL", Value: "https://clawvisor.company.internal:18791"},
		},
	}
	var buf bytes.Buffer
	if err := EmitComposeIsolationOverride(&buf, plan); err != nil {
		t.Fatalf("emit: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, `CLAWVISOR_HOST_TARGET: "clawvisor.company.internal"`) {
		t.Errorf("expected CLAWVISOR_HOST_TARGET to be the hostname, got:\n%s", out)
	}
	// Strip leading comments before asserting on emitted YAML — the header
	// comment block legitimately mentions extra_hosts as documentation.
	yamlOnly := strings.SplitN(out, "services:\n", 2)
	if len(yamlOnly) != 2 {
		t.Fatalf("could not find `services:` in output: %s", out)
	}
	if strings.Contains(yamlOnly[1], "extra_hosts") {
		t.Errorf("user service should not emit extra_hosts (compose forbids on network_mode: service:): %s", out)
	}
}
