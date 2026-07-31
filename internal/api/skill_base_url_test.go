package api

import (
	"crypto/tls"
	"net/http/httptest"
	"testing"
)

// TestSkillBaseURLMatchesInstallerScheme — the scheme baked into a rendered
// SKILL.md has to agree with the one the installer writes into the Claude Code
// permission allowlist (InstallerHandler.resolveAppURL). When they disagreed,
// the skill told agents to call http:// while the allow rule covered https://,
// so the rule matched nothing and every skill-directed call prompted for
// approval. That is merely annoying interactively and fatal in the
// non-interactive dry run, which has no way to answer a prompt -- and it had
// agents sending a high-privilege token over plaintext.
//
// Cloud Run and every other TLS-terminating proxy leave r.TLS nil and signal
// the original scheme in X-Forwarded-Proto, so that header is the only thing
// standing between this and http:// in production.
func TestSkillBaseURLMatchesInstallerScheme(t *testing.T) {
	for _, tc := range []struct {
		name      string
		tls       bool
		fwdProto  string
		host      string
		viaRelay  bool
		daemonID  string
		relayHost string
		want      string
	}{{
		name:     "behind a TLS-terminating proxy",
		fwdProto: "https",
		host:     "app.staging.clawvisor.com",
		want:     "https://app.staging.clawvisor.com",
	}, {
		name: "direct TLS",
		tls:  true,
		host: "app.clawvisor.com",
		want: "https://app.clawvisor.com",
	}, {
		name: "plain local http",
		host: "localhost:25297",
		want: "http://localhost:25297",
	}, {
		// An explicit http header wins over the nil-TLS default landing on the
		// same answer -- the header is authoritative either way.
		name:     "proxy reports http",
		fwdProto: "http",
		host:     "localhost:25297",
		want:     "http://localhost:25297",
	}, {
		// Relay-served installs address the daemon through the relay and are
		// always https, regardless of how this hop arrived.
		name:      "via relay",
		viaRelay:  true,
		daemonID:  "abc123",
		relayHost: "relay.clawvisor.com",
		host:      "ignored",
		want:      "https://relay.clawvisor.com/d/abc123",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/skill/SKILL.md", nil)
			r.Host = tc.host
			if tc.tls {
				r.TLS = &tls.ConnectionState{}
			}
			if tc.fwdProto != "" {
				r.Header.Set("X-Forwarded-Proto", tc.fwdProto)
			}
			if got := skillBaseURL(r, tc.viaRelay, tc.daemonID, tc.relayHost); got != tc.want {
				t.Errorf("skillBaseURL = %q, want %q", got, tc.want)
			}
		})
	}
}
