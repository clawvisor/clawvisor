package handlers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestValidateTokenEndpoint covers the scheme/host policy that gates which
// URLs the OAuth and PKCE flows are willing to dial. Combined with
// ssrfSafeClient's connect-time IP check, this is the SSRF defense for
// adapter-supplied token endpoints.
func TestValidateTokenEndpoint(t *testing.T) {
	cases := []struct {
		name    string
		url     string
		wantErr bool
	}{
		{"valid https", "https://provider.example.com/oauth/token", false},
		{"localhost http allowed for dev", "http://localhost:8080/token", false},
		{"127.0.0.1 http allowed for dev", "http://127.0.0.1:8080/token", false},
		{"::1 http allowed for dev", "http://[::1]:8080/token", false},
		{"http external rejected", "http://provider.example.com/token", true},
		{"empty rejected", "", true},
		{"missing host rejected", "https://", true},
		{"unsupported scheme rejected", "file:///etc/passwd", true},
		{"gopher rejected", "gopher://provider.example.com/", true},
		{"userinfo rejected", "https://user:pass@provider.example.com/token", true},
		{"unparseable", "://broken", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateTokenEndpoint(tc.url)
			if (err != nil) != tc.wantErr {
				t.Fatalf("validateTokenEndpoint(%q) err=%v wantErr=%v", tc.url, err, tc.wantErr)
			}
		})
	}
}

// TestSSRFSafeClient_BlocksPrivateIPs verifies that the spec-fetch HTTP client
// refuses to dial RFC1918 / link-local / loopback / cloud-metadata addresses,
// even when the URL is otherwise well-formed and reachable. OAuth uses a
// looser sibling client (ssrfSafeOAuthClient) that exempts loopback to match
// validateTokenEndpoint — covered by TestSSRFSafeOAuthClient_AllowsLoopback.
func TestSSRFSafeClient_BlocksPrivateIPs(t *testing.T) {
	// Use a real listener so we know there's nothing to TOCTOU with — the
	// dialer must reject before any TCP connect is attempted.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	blocked := []string{
		"http://127.0.0.1:1/x",          // loopback
		"http://10.0.0.1:80/x",          // RFC1918
		"http://192.168.1.1:80/x",       // RFC1918
		"http://169.254.169.254/latest", // cloud metadata
		"http://172.16.0.1:80/x",        // RFC1918
		"http://[::1]:1/x",              // ipv6 loopback
	}
	for _, raw := range blocked {
		t.Run(raw, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodGet, raw, nil)
			if err != nil {
				t.Fatalf("NewRequest: %v", err)
			}
			resp, err := ssrfSafeClient.Do(req)
			if err == nil {
				resp.Body.Close()
				t.Fatalf("expected SSRF-safe client to reject %q, got status %d", raw, resp.StatusCode)
			}
			// The dialer reports either "blocked IP" or "no safe IPs found".
			if !strings.Contains(err.Error(), "blocked IP") && !strings.Contains(err.Error(), "no safe IPs") {
				t.Fatalf("unexpected error for %q: %v", raw, err)
			}
		})
	}
}

// TestSSRFSafeOAuthClient_AllowsLoopback ensures that the OAuth-specific
// client lets http://localhost / 127.0.0.1 / ::1 endpoints through (since
// validateTokenEndpoint already allows them), while still rejecting RFC1918,
// link-local, and cloud-metadata ranges.
func TestSSRFSafeOAuthClient_AllowsLoopback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	t.Run("loopback allowed", func(t *testing.T) {
		req, err := http.NewRequest(http.MethodGet, srv.URL, nil)
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		resp, err := ssrfSafeOAuthClient.Do(req)
		if err != nil {
			t.Fatalf("oauth client should accept loopback %q: %v", srv.URL, err)
		}
		resp.Body.Close()
	})

	blocked := []string{
		"http://10.0.0.1:80/x",          // RFC1918
		"http://192.168.1.1:80/x",       // RFC1918
		"http://169.254.169.254/latest", // cloud metadata
		"http://172.16.0.1:80/x",        // RFC1918
	}
	for _, raw := range blocked {
		t.Run("blocked "+raw, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodGet, raw, nil)
			if err != nil {
				t.Fatalf("NewRequest: %v", err)
			}
			resp, err := ssrfSafeOAuthClient.Do(req)
			if err == nil {
				resp.Body.Close()
				t.Fatalf("expected oauth client to reject %q, got status %d", raw, resp.StatusCode)
			}
			if !strings.Contains(err.Error(), "blocked IP") && !strings.Contains(err.Error(), "no safe IPs") {
				t.Fatalf("unexpected error for %q: %v", raw, err)
			}
		})
	}
}
