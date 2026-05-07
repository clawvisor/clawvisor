package api

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

func mustCIDR(t *testing.T, s string) *net.IPNet {
	t.Helper()
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		t.Fatalf("ParseCIDR(%q): %v", s, err)
	}
	return n
}

func TestClientIPFromRequest(t *testing.T) {
	trusted := []*net.IPNet{
		mustCIDR(t, "10.0.0.0/8"),
		mustCIDR(t, "127.0.0.1/32"),
	}

	cases := []struct {
		name       string
		remoteAddr string
		xff        string
		trusted    []*net.IPNet
		want       string
	}{
		{
			name:       "no trusted proxies — use RemoteAddr verbatim",
			remoteAddr: "203.0.113.5:443",
			xff:        "1.2.3.4",
			trusted:    nil,
			want:       "203.0.113.5",
		},
		{
			name:       "trusted-set defined but peer is untrusted — ignore XFF",
			remoteAddr: "203.0.113.5:443",
			xff:        "1.2.3.4",
			trusted:    trusted,
			want:       "203.0.113.5",
		},
		{
			name:       "peer is trusted, no XFF — fall back to peer host",
			remoteAddr: "10.0.0.5:443",
			xff:        "",
			trusted:    trusted,
			want:       "10.0.0.5",
		},
		{
			name:       "single XFF entry from trusted peer — use that entry",
			remoteAddr: "10.0.0.5:443",
			xff:        "203.0.113.99",
			trusted:    trusted,
			want:       "203.0.113.99",
		},
		{
			name:       "multi-hop XFF, last is trusted proxy — pick rightmost untrusted",
			remoteAddr: "10.0.0.5:443",
			xff:        "203.0.113.99, 10.0.0.99, 10.0.0.5",
			trusted:    trusted,
			want:       "203.0.113.99",
		},
		{
			name:       "all XFF entries are trusted — fall back to peer",
			remoteAddr: "10.0.0.5:443",
			xff:        "10.0.0.1, 10.0.0.2",
			trusted:    trusted,
			want:       "10.0.0.5",
		},
		{
			name:       "junk XFF entries are skipped",
			remoteAddr: "10.0.0.5:443",
			xff:        "not-an-ip, 203.0.113.7",
			trusted:    trusted,
			want:       "203.0.113.7",
		},
		// IPv6 coverage. Bracketed IPv6 RemoteAddr must round-trip and match
		// IPv6 trusted CIDRs the same way IPv4 does.
		{
			name:       "ipv6 trusted peer with ipv6 XFF — pick rightmost untrusted",
			remoteAddr: "[::1]:443",
			xff:        "2001:db8::42, ::1",
			trusted: []*net.IPNet{
				mustCIDR(t, "::1/128"),
			},
			want: "2001:db8::42",
		},
		{
			name:       "ipv6 untrusted peer ignores XFF",
			remoteAddr: "[2001:db8::99]:443",
			xff:        "10.0.0.5",
			trusted: []*net.IPNet{
				mustCIDR(t, "10.0.0.0/8"),
			},
			want: "2001:db8::99",
		},
		// Spoofing surface: an attacker on the public internet sets
		// X-Forwarded-For: 10.0.0.1 hoping the daemon honors it. With a
		// trusted-proxies set defined but the peer NOT in it, the header
		// is ignored. Without this the rate-limit key would be forgable.
		{
			name:       "untrusted public peer cannot spoof XFF",
			remoteAddr: "203.0.113.99:443",
			xff:        "10.0.0.1",
			trusted:    trusted,
			want:       "203.0.113.99",
		},
		// IPv4-mapped IPv6 (::ffff:10.0.0.5) commonly appears behind
		// dual-stack proxies. A CIDR matching the IPv4 form should still
		// recognize the mapped form.
		{
			name:       "ipv4-mapped ipv6 in XFF is honored",
			remoteAddr: "10.0.0.5:443",
			xff:        "::ffff:203.0.113.7",
			trusted:    trusted,
			want:       "::ffff:203.0.113.7",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.RemoteAddr = tc.remoteAddr
			if tc.xff != "" {
				r.Header.Set("X-Forwarded-For", tc.xff)
			}
			got := clientIPFromRequest(r, tc.trusted)
			if got != tc.want {
				t.Fatalf("clientIPFromRequest = %q, want %q", got, tc.want)
			}
		})
	}
}
