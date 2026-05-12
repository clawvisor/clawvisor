package inspector

import (
	"encoding/json"
	"strings"
	"testing"
)

// Regression: real models emit benign curl flags like `-s`, `-sS`,
// `--silent`, `--max-time 30`, etc. The parser previously refused
// anything outside `-X` and `-H` as ambiguous, blocking the rewrite
// path entirely.
func TestParseBashCurl_AcceptsBenignFlags(t *testing.T) {
	cases := []struct {
		name string
		cmd  string
	}{
		{"silent_short", `curl -s -H 'Authorization: Bearer autovault_github_xxx' https://api.github.com/user`},
		{"silent_show_error", `curl -sS -H 'Authorization: Bearer autovault_github_xxx' https://api.github.com/user`},
		{"silent_show_error_fail", `curl -fsS -H 'Authorization: Bearer autovault_github_xxx' https://api.github.com/user`},
		{"silent_long", `curl --silent -H 'Authorization: Bearer autovault_github_xxx' https://api.github.com/user`},
		{"include", `curl -i -H 'Authorization: Bearer autovault_github_xxx' https://api.github.com/user`},
		{"compressed", `curl --compressed -H 'Authorization: Bearer autovault_github_xxx' https://api.github.com/user`},
		{"max_time_long", `curl --max-time 30 -H 'Authorization: Bearer autovault_github_xxx' https://api.github.com/user`},
		{"max_time_short", `curl -m 30 -H 'Authorization: Bearer autovault_github_xxx' https://api.github.com/user`},
		{"user_agent", `curl -A 'clawvisor-smoke/1.0' -H 'Authorization: Bearer autovault_github_xxx' https://api.github.com/user`},
		{"combined_with_method", `curl -sS -X POST -H 'Authorization: Bearer autovault_github_xxx' https://api.github.com/repos/x/y/issues`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tu := ToolUse{
				ID:    "toolu_flags",
				Name:  "Bash",
				Input: json.RawMessage(`{"cmd":` + jsonString(tc.cmd) + `}`),
			}
			got, ok := DefaultParser{}.Parse(tu)
			if !ok {
				t.Fatalf("parser fell through; verdict=%+v", got)
			}
			if !got.IsAPICall {
				t.Fatalf("expected IsAPICall=true for %q; got reason=%q", tc.cmd, got.Reason)
			}
			if got.Ambiguous {
				t.Fatalf("expected non-ambiguous for %q; got reason=%q", tc.cmd, got.Reason)
			}
		})
	}
}

// Negative: dangerous flags should still bounce to ambiguous so the
// rewriter refuses the call. `-L` (follow redirects), `-k` (TLS bypass),
// `-x` (proxy override), and request-body flags fall into this set.
func TestParseBashCurl_RejectsDangerousFlags(t *testing.T) {
	cases := []struct {
		name string
		cmd  string
	}{
		{"follow_location", `curl -L -H 'Authorization: Bearer autovault_github_xxx' https://api.github.com/user`},
		{"insecure", `curl -k -H 'Authorization: Bearer autovault_github_xxx' https://api.github.com/user`},
		{"proxy", `curl -x http://proxy.example -H 'Authorization: Bearer autovault_github_xxx' https://api.github.com/user`},
		{"data_short", `curl -d 'evil' -H 'Authorization: Bearer autovault_github_xxx' https://api.github.com/user`},
		{"data_long", `curl --data-raw 'evil' -H 'Authorization: Bearer autovault_github_xxx' https://api.github.com/user`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tu := ToolUse{
				ID:    "toolu_dangerous",
				Name:  "Bash",
				Input: json.RawMessage(`{"cmd":` + jsonString(tc.cmd) + `}`),
			}
			got, _ := DefaultParser{}.Parse(tu)
			if got.IsAPICall {
				t.Fatalf("expected dangerous flag %q to remain ambiguous, got IsAPICall=true", tc.cmd)
			}
		})
	}
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// isSafeBoolCurlFlag's short-flag cluster handling must only accept
// clusters where every character is in the safe-short set. A single
// letter outside the set in a cluster must reject the whole token.
func TestIsSafeBoolCurlFlag_ShortFlagClusters(t *testing.T) {
	cases := map[string]bool{
		"-s":   true,
		"-sS":  true,
		"-fsS": true,
		"-sSf": true,
		"-Lf":  false, // -L (location) is refused, so the cluster is refused
		"-sk":  false, // -k (insecure) is refused
		"-d":   false, // -d alone isn't in the bool set
	}
	for tok, want := range cases {
		got := isSafeBoolCurlFlag(tok)
		if got != want {
			t.Errorf("isSafeBoolCurlFlag(%q) = %v, want %v", tok, got, want)
		}
	}
}

// Sanity: the example error the user hit (`bash: unknown curl flag -s`)
// no longer fires for `curl -s`.
func TestParseBashCurl_DashSNoLongerAmbiguous(t *testing.T) {
	tu := ToolUse{
		ID:   "toolu_dash_s",
		Name: "Bash",
		Input: json.RawMessage(
			`{"cmd":"curl -s -H 'Authorization: Bearer autovault_github_abc' https://api.github.com/user"}`,
		),
	}
	got, ok := DefaultParser{}.Parse(tu)
	if !ok || got.Ambiguous {
		t.Fatalf("expected -s to be accepted; got ambiguous=%v reason=%q", got.Ambiguous, got.Reason)
	}
	if strings.Contains(got.Reason, "unknown curl flag") {
		t.Errorf("reason still mentions unknown curl flag: %q", got.Reason)
	}
}
