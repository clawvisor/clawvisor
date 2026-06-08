package scriptjudge

import (
	"context"
	"strings"
	"testing"
)

// TestExtractToken covers the regex that surfaces a cv-script-… token
// from arbitrary tool_use text. The judge needs this extraction to
// confirm the agent isn't mentioning the prefix in unrelated prose,
// and to pass the token through to its prompt.
func TestExtractToken(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"literal header", `-H 'X-Clawvisor-Caller: Bearer cv-script-abc123'`, "cv-script-abc123"},
		{"variable assignment", `C='X-Clawvisor-Caller: Bearer cv-script-mc3ub6r7ri7i2f4oa5vqwrk3by'`, "cv-script-mc3ub6r7ri7i2f4oa5vqwrk3by"},
		{"first of many", "echo cv-script-aaa; echo cv-script-bbb", "cv-script-aaa"},
		{"none present", "no token in this text", ""},
		{"prefix only (no token body)", "cv-script-", ""},
		{"uppercase suffix not matched (token alphabet is lowercase+digits)", "cv-script-ABC", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ExtractToken(tc.in)
			if got != tc.want {
				t.Fatalf("ExtractToken(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestRedactSensitive_StripsTokensAndPlaceholders pins the redaction
// shape: cv-script tokens and autovault placeholders never leave the
// proxy boundary intact when the judge forwards tool_use input to a
// third-party LLM provider, and don't end up persisted in audit rows
// via wrapped LLM-client error strings.
func TestRedactSensitive_StripsTokensAndPlaceholders(t *testing.T) {
	cases := []struct {
		name      string
		in        string
		mustKeep  []string
		mustStrip []string
	}{
		{
			name:      "cv-script token",
			in:        `-H 'X-Clawvisor-Caller: Bearer cv-script-mc3ub6r7ri7i2f4oa5vqwrk3by'`,
			mustKeep:  []string{"X-Clawvisor-Caller", "Bearer", "cv-script-<redacted>"},
			mustStrip: []string{"cv-script-mc3ub6r7ri7i2f4oa5vqwrk3by"},
		},
		{
			name:      "autovault placeholder",
			in:        `Authorization: Bearer autovault_google_gmail_eric_xxxxxxxx`,
			mustKeep:  []string{"Bearer", "autovault_<redacted>"},
			mustStrip: []string{"autovault_google_gmail_eric_xxxxxxxx"},
		},
		{
			name:      "both in one curl",
			in:        `curl -H 'X-Clawvisor-Caller: Bearer cv-script-abc' -H 'Authorization: Bearer autovault_gh_xyz' https://localhost:25297/api/proxy/x`,
			mustKeep:  []string{"curl", "https://localhost:25297/api/proxy/x", "cv-script-<redacted>", "autovault_<redacted>"},
			mustStrip: []string{"cv-script-abc", "autovault_gh_xyz"},
		},
		{
			name:     "no sensitive content unchanged",
			in:       `curl https://api.github.com/repos/x/y/issues`,
			mustKeep: []string{"curl", "https://api.github.com/repos/x/y/issues"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := RedactSensitive(tc.in)
			for _, want := range tc.mustKeep {
				if !strings.Contains(got, want) {
					t.Errorf("RedactSensitive output missing %q: %s", want, got)
				}
			}
			for _, banned := range tc.mustStrip {
				if strings.Contains(got, banned) {
					t.Errorf("RedactSensitive leaked %q: %s", banned, got)
				}
			}
		})
	}
}

// TestNoop_AlwaysErrors anchors the no-op behavior so callers (and
// the production fallback path) can distinguish "no judge configured"
// from "judge said block."
func TestNoop_AlwaysErrors(t *testing.T) {
	v, err := (Noop{}).Judge(context.Background(), Input{})
	if err == nil {
		t.Fatalf("Noop.Judge: expected error, got verdict %+v", v)
	}
	if !strings.Contains(err.Error(), "not configured") {
		t.Errorf("error %q should mention \"not configured\"", err)
	}
}
