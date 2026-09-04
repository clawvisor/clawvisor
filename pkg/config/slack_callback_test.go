package config

import "testing"

func TestSlackCallbackBaseURL(t *testing.T) {
	for _, tc := range []struct {
		name   string
		slack  string
		server string
		want   string
	}{
		{
			name:   "falls back to the server public URL",
			server: "https://app.clawvisor.com",
			want:   "https://app.clawvisor.com",
		},
		{
			// The local-dev case: a tunnel serves Slack's callbacks while
			// server.public_url keeps addressing the dashboard.
			name:   "override wins over the server public URL",
			slack:  "https://dev-box.tailnet.ts.net",
			server: "https://dev.clawvisor.com:8443",
			want:   "https://dev-box.tailnet.ts.net",
		},
		{
			name:   "trailing slashes are trimmed so paths do not double up",
			slack:  "https://dev-box.tailnet.ts.net/",
			server: "https://dev.clawvisor.com:8443",
			want:   "https://dev-box.tailnet.ts.net",
		},
		{
			name:   "surrounding whitespace is ignored",
			slack:  "  https://dev-box.tailnet.ts.net  ",
			server: "",
			want:   "https://dev-box.tailnet.ts.net",
		},
		{
			// A whitespace-only override is not an override. Treating it as
			// one would disable Slack entirely despite a valid
			// server.public_url.
			name:   "whitespace-only override falls back to the server public URL",
			slack:  "   ",
			server: "https://app.clawvisor.com",
			want:   "https://app.clawvisor.com",
		},
		{
			name:   "whitespace-only server public URL is not a callback origin",
			slack:  "",
			server: "  \t ",
			want:   "",
		},
		{
			// Neither set must disable Slack rather than build a relative
			// redirect URI that Slack would reject at install time.
			name: "empty when neither is set",
			want: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := SlackConfig{PublicURL: tc.slack}
			if got := c.CallbackBaseURL(tc.server); got != tc.want {
				t.Fatalf("CallbackBaseURL() = %q, want %q", got, tc.want)
			}
		})
	}
}

// Enabled covers only the app credentials; the callback origin is a separate
// gate, so a deployment with credentials but no reachable URL must not read
// as ready.
func TestSlackEnabledIgnoresCallbackURL(t *testing.T) {
	full := SlackConfig{ClientID: "id", ClientSecret: "secret", SigningSecret: "sign"}
	if !full.Enabled() {
		t.Fatal("fully-credentialled config reported not enabled")
	}
	if full.CallbackBaseURL("") != "" {
		t.Fatal("expected no callback base URL when neither source is set")
	}

	for _, missing := range []SlackConfig{
		{ClientSecret: "secret", SigningSecret: "sign"},
		{ClientID: "id", SigningSecret: "sign"},
		{ClientID: "id", ClientSecret: "secret"},
	} {
		if missing.Enabled() {
			t.Fatalf("partial credentials reported enabled: %+v", missing)
		}
	}
}

// Terraform provisions the Secret Manager slots with a "REPLACE_ME" body so
// Cloud Run can resolve the reference before anyone pastes real values.
// Treating that as configured would advertise Slack in the UI and then reject
// every interaction as an invalid signature, with nothing pointing at
// unfinished setup as the cause.
func TestSlackEnabled_RejectsTerraformPlaceholder(t *testing.T) {
	real := SlackConfig{ClientID: "id", ClientSecret: "secret", SigningSecret: "sign"}
	if !real.Enabled() {
		t.Fatal("fully-configured Slack reported disabled")
	}

	for _, tc := range []struct {
		name string
		cfg  SlackConfig
	}{
		{"client id is the placeholder", SlackConfig{ClientID: "REPLACE_ME", ClientSecret: "s", SigningSecret: "g"}},
		{"client secret is the placeholder", SlackConfig{ClientID: "i", ClientSecret: "REPLACE_ME", SigningSecret: "g"}},
		{"signing secret is the placeholder", SlackConfig{ClientID: "i", ClientSecret: "s", SigningSecret: "REPLACE_ME"}},
		{"all three", SlackConfig{ClientID: "REPLACE_ME", ClientSecret: "REPLACE_ME", SigningSecret: "REPLACE_ME"}},
		// Casing and stray whitespace must not smuggle it past the guard.
		{"lowercase", SlackConfig{ClientID: "i", ClientSecret: "s", SigningSecret: "replace_me"}},
		{"padded", SlackConfig{ClientID: "i", ClientSecret: "s", SigningSecret: "  REPLACE_ME  "}},
		{"whitespace-only value", SlackConfig{ClientID: "i", ClientSecret: "   ", SigningSecret: "g"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.cfg.Enabled() {
				t.Fatal("placeholder or blank credential reported as enabled")
			}
		})
	}
}
