package handlers

import (
	"crypto/tls"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestForwardedSchemeRejectsAnythingButHTTPAndHTTPS — X-Forwarded-Proto is
// attacker-controllable unless the edge proxy overwrites it rather than
// appending, and callers interpolate the result into content that is then
// executed or followed:
//
//   - resolveAppURL -> {{.AppURL}} -> APP_URL='...' in the installer, a
//     text/template (no escaping) that users run via `curl | sh`. Reflecting
//     the header verbatim rendered APP_URL=''; echo MARKER; '://host', which
//     executes -- arbitrary command injection in the install path.
//   - skillBaseURL -> the base URL baked into a served SKILL.md, i.e. into
//     instructions an autonomous agent acts on.
//
// So the scheme is an allowlist, not a passthrough. A rejected value must fall
// back to the transport-derived scheme rather than propagating.
func TestForwardedSchemeRejectsAnythingButHTTPAndHTTPS(t *testing.T) {
	for _, tc := range []struct {
		name, header string
		tls          bool
		want         string
	}{
		{name: "no header, plain", want: "http"},
		{name: "no header, TLS", tls: true, want: "https"},
		{name: "https", header: "https", want: "https"},
		{name: "http", header: "http", want: "http"},
		{name: "uppercase", header: "HTTPS", want: "https"},
		{name: "padded", header: "  https  ", want: "https"},
		// Chained proxies append; the client-facing hop is first.
		{name: "chained", header: "https, http", want: "https"},
		{name: "chained padded", header: " https , http", want: "https"},
		// The injection that made this a security fix rather than a cleanup.
		{name: "shell breakout", header: "'; echo INJECTED; '", want: "http"},
		{name: "shell breakout over TLS", header: "'; echo INJECTED; '", tls: true, want: "https"},
		// Neither a scheme nor shell metacharacters, but still not ours.
		{name: "unknown scheme", header: "gopher", want: "http"},
		{name: "newline", header: "https\r\nX-Evil: 1", want: "http"},
		{name: "empty", header: "", want: "http"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/", nil)
			if tc.tls {
				r.TLS = &tls.ConnectionState{}
			}
			if tc.header != "" {
				r.Header.Set("X-Forwarded-Proto", tc.header)
			}
			got := ForwardedScheme(r)
			if got != tc.want {
				t.Errorf("ForwardedScheme = %q, want %q", got, tc.want)
			}
			// Belt and braces: whatever we return is concatenated straight into
			// a URL, so it must never carry anything that could break out of
			// the quoting at the other end.
			if strings.ContainsAny(got, "'\"`$;\r\n \\") {
				t.Errorf("ForwardedScheme returned unsafe value %q", got)
			}
		})
	}
}
