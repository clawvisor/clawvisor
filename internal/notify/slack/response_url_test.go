package slack

import "testing"

// response_url arrives inside the interaction payload, so it is
// attacker-chosen input to an outbound request from inside the deployment's
// network. Hostname-locking it keeps a forged payload from reaching cloud
// metadata or internal services.
func TestValidResponseURL(t *testing.T) {
	for _, tc := range []struct {
		name string
		url  string
		want bool
	}{
		{"genuine Slack response_url", "https://hooks.slack.com/actions/T0001/123/abcXYZ", true},
		{"host casing is ignored", "https://Hooks.Slack.Com/actions/T0001/123/abc", true},

		{"empty", "", false},
		{"http downgrade", "http://hooks.slack.com/actions/T0001/123/abc", false},
		{"unrelated host", "https://evil.example.com/actions/T0001/123/abc", false},
		// The classic SSRF targets an outbound request from inside a cloud
		// network can reach.
		{"cloud metadata", "http://169.254.169.254/latest/meta-data/", false},
		{"loopback", "http://127.0.0.1:8080/internal", false},
		{"internal hostname", "https://redis.internal:6379/", false},
		// A suffix match would let these through.
		{"suffix impersonation", "https://hooks.slack.com.evil.example/x", false},
		{"prefix impersonation", "https://evilhooks.slack.com.attacker/x", false},
		// Userinfo can make a hostile host look like Slack to a careless
		// parser.
		{"userinfo trick", "https://hooks.slack.com@evil.example/x", false},
		{"scheme-less", "hooks.slack.com/actions/T0001/123/abc", false},
		{"garbage", "://not a url", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := validResponseURL(tc.url); got != tc.want {
				t.Fatalf("validResponseURL(%q) = %v, want %v", tc.url, got, tc.want)
			}
		})
	}
}
