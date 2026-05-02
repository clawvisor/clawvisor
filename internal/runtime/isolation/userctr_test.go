package isolation

import (
	"strings"
	"testing"
)

func TestCheckUserArgsRejectsConflictingFlags(t *testing.T) {
	cases := [][]string{
		{"--network", "bridge", "alpine"},
		{"--network=host", "alpine"},
		{"--add-host", "foo:1.2.3.4", "alpine"},
		{"--add-host=foo:1.2.3.4", "alpine"},
		{"--dns", "8.8.8.8", "alpine"},
		{"--dns-search", "example.com", "alpine"},
		{"--net", "host", "alpine"},
	}
	for _, args := range cases {
		err := CheckUserArgs(args)
		if err == nil {
			t.Errorf("CheckUserArgs(%v) returned nil; expected error", args)
		}
	}
}

func TestCheckUserArgsAllowsBenignFlags(t *testing.T) {
	cases := [][]string{
		{"--rm", "-it", "alpine", "sh"},
		{"-v", "/tmp:/tmp", "alpine"},
		{"--workdir", "/app", "alpine"},
		{},
	}
	for _, args := range cases {
		if err := CheckUserArgs(args); err != nil {
			t.Errorf("CheckUserArgs(%v) returned %v; expected nil", args, err)
		}
	}
}

func TestRewriteAPIURL(t *testing.T) {
	tests := []struct {
		name     string
		baseURL  string
		gateway  string
		port     int
		wantURL  string
		wantKind HostKind
		wantHost string
	}{
		{
			name:     "loopback host",
			baseURL:  "http://127.0.0.1:18789/",
			gateway:  "172.20.0.1",
			port:     34567,
			wantURL:  "http://172.20.0.1:34567/",
			wantKind: HostKindLoopback,
		},
		{
			name:     "loopback name",
			baseURL:  "http://localhost:18789",
			gateway:  "172.20.0.1",
			port:     34567,
			wantURL:  "http://172.20.0.1:34567",
			wantKind: HostKindLoopback,
		},
		{
			name:     "ipv6 loopback",
			baseURL:  "http://[::1]:18789/",
			gateway:  "172.20.0.1",
			port:     34567,
			wantURL:  "http://172.20.0.1:34567/",
			wantKind: HostKindLoopback,
		},
		{
			name:     "dns hostname https with path",
			baseURL:  "https://clawvisor.company.internal/api",
			gateway:  "172.20.0.1",
			port:     34567,
			wantURL:  "https://clawvisor.company.internal:34567/api",
			wantKind: HostKindDNSName,
			wantHost: "clawvisor.company.internal",
		},
		{
			name:     "non-loopback ip literal",
			baseURL:  "http://10.0.5.50:8080/",
			gateway:  "172.20.0.1",
			port:     34567,
			wantURL:  "http://172.20.0.1:34567/",
			wantKind: HostKindIPLiteral,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := RewriteAPIURL(tc.baseURL, tc.gateway, tc.port)
			if err != nil {
				t.Fatalf("RewriteAPIURL err: %v", err)
			}
			if got.URL != tc.wantURL {
				t.Errorf("URL: got %q want %q", got.URL, tc.wantURL)
			}
			if got.Kind != tc.wantKind {
				t.Errorf("Kind: got %v want %v", got.Kind, tc.wantKind)
			}
			if got.Hostname != tc.wantHost {
				t.Errorf("Hostname: got %q want %q", got.Hostname, tc.wantHost)
			}
		})
	}
}

func TestResolveUpstream(t *testing.T) {
	tests := []struct {
		baseURL string
		want    string
	}{
		{"http://127.0.0.1:18789/", "127.0.0.1:18789"},
		{"http://example.com/api", "example.com:80"},
		{"https://example.com/api", "example.com:443"},
		{"https://example.com:8443/api", "example.com:8443"},
	}
	for _, tc := range tests {
		got, err := ResolveUpstream(tc.baseURL)
		if err != nil {
			t.Fatalf("ResolveUpstream(%q) err: %v", tc.baseURL, err)
		}
		if got != tc.want {
			t.Errorf("ResolveUpstream(%q) = %q want %q", tc.baseURL, got, tc.want)
		}
	}
}

func TestRewriteAPIURLRejectsEmptyHost(t *testing.T) {
	if _, err := RewriteAPIURL("http://", "172.20.0.1", 34567); err == nil {
		t.Fatal("expected error for empty hostname")
	}
	if _, err := RewriteAPIURL("not a url", "172.20.0.1", 34567); err == nil {
		t.Fatal("expected error for malformed URL")
	}
}

func TestConflictingFlagsListIsExhaustive(t *testing.T) {
	// The conflict list must include every doc-mentioned flag so a regression
	// rename in init() doesn't drop coverage silently.
	required := []string{"--network", "--net", "--add-host", "--dns", "--dns-search"}
	have := strings.Join(ConflictingFlags, ",")
	for _, r := range required {
		if !strings.Contains(have, r) {
			t.Errorf("ConflictingFlags missing %q (have: %s)", r, have)
		}
	}
}
