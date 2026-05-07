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
