package handlers

import (
	"fmt"
	"os/exec"
	"strings"
	"testing"
)

// shellInstallerVariants is every (target, route) pair that renders a shell
// script. Kept as one table so a new route variant gets syntax-checked by
// construction rather than by remembering to add it.
func shellInstallerVariants() []struct{ name, target, query string } {
	return []struct{ name, target, query string }{
		{"claude-code default (skill-only)", "claude-code", "claim=ABCDEFGHIJ"},
		{"claude-code route=skill-only", "claude-code", "claim=ABCDEFGHIJ&route=skill-only"},
		{"claude-code route=proxy", "claude-code", "claim=ABCDEFGHIJ&route=proxy"},
		{"claude-code route=subscription", "claude-code", "claim=ABCDEFGHIJ&route=subscription"},
		{"codex default (skill-only)", "codex", "claim=CLAIMCODE0"},
		{"codex route=skill-only", "codex", "claim=CLAIMCODE0&route=skill-only"},
		{"codex route=proxy", "codex", "claim=CLAIMCODE0&route=proxy"},
	}
}

// TestInstallerScriptsAreValidShell parses every rendered installer with
// `sh -n`. Until this existed, nothing in the repo checked that the scripts we
// pipe into a user's shell were even syntactically valid — every other assertion
// is a substring match, which cannot catch an unbalanced heredoc, an unclosed
// `if`, or a stray `fi`. Those are exactly the mistakes a large template edit
// makes, and the failure mode is a script that dies partway through after having
// already half-modified the user's config.
//
// `sh -n` is POSIX and present everywhere the tests run, so this needs no new
// dependency. It is a parse-only check: nothing is executed.
func TestInstallerScriptsAreValidShell(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skipf("no sh on PATH: %v", err)
	}
	h := NewInstallerHandler("relay.example.com", "daemon1", false,
		"https://llm.example.com", "https://app.example.com")

	for _, v := range shellInstallerVariants() {
		t.Run(v.name, func(t *testing.T) {
			body := installerGetShellQuery(t, h, v.target, v.query)

			cmd := exec.Command("sh", "-n")
			cmd.Stdin = strings.NewReader(body)
			out, err := cmd.CombinedOutput()
			if err != nil {
				// Number the lines so the parser's complaint is actionable —
				// "syntax error near line 214" is useless against a body you
				// cannot see.
				var b strings.Builder
				for i, line := range strings.Split(body, "\n") {
					fmt.Fprintf(&b, "%d\t%s\n", i+1, line)
				}
				t.Fatalf("`sh -n` rejected the rendered script: %v\n%s\n--- script ---\n%s",
					err, out, b.String())
			}
		})
	}
}
