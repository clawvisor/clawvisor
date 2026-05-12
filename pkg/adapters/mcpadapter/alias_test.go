package mcpadapter

import "testing"

func TestNormalizeAlias(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		// Real-world inputs we've seen on prod.
		{"email passes through", "eric@clawvisor.com", "eric@clawvisor.com"},
		{"org with apostrophe and spaces", "Eric Levine's Org", "eric-levines-org"},

		// Single-word identities — common for GitHub-style logins.
		{"plain lowercase", "octocat", "octocat"},
		{"plain mixed case", "OctoCat", "octocat"},

		// Spaces collapse to a single dash.
		{"consecutive spaces collapse", "Acme  Corp   LLC", "acme-corp-llc"},
		{"tabs and newlines treated as space", "Acme\tCorp\nLLC", "acme-corp-llc"},

		// Edge cases.
		{"empty input stays empty", "", ""},
		{"only invalid chars yields empty", "🚀✨", ""},
		{"strips leading/trailing dashes", "  hello  ", "hello"},
		{"strips leading/trailing dots", "...hello...", "hello"},

		// Allowed punctuation passes through.
		{"underscore allowed", "user_name", "user_name"},
		{"plus allowed for tagged emails", "user+tag@x.com", "user+tag@x.com"},
		{"dot in middle preserved", "first.last", "first.last"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizeAlias(tc.in)
			if got != tc.want {
				t.Errorf("normalizeAlias(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
