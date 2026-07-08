//go:build realclient

package realclient

import (
	"regexp"
	"testing"
	"time"
)

// The Codex (OpenAI) realclient scenarios — the OPENAI_BASE_URL variants of the
// attribution + key-injection checks. Gated on CLAWVISOR_REALCLIENT=1 +
// OPENAI_API_KEY; skip cleanly otherwise. Cheapest model, single-shot prompts.

const codexAgentModel = "gpt-4o-mini"

// codexReady matches a stable Codex TUI landmark (no loose ">" that would match
// the launching shell prompt). Like claudeReady these strings track a
// fast-moving TUI and may need per-version tuning — the lane is advisory.
//
// NB (drift): unlike Claude Code, the real `clawvisor agent lite -- codex` path
// routes Codex through proxy-lite via injected `-c model_providers.clawvisor.*`
// TOML overrides + a CLAWVISOR_AGENT_TOKEN header (see
// internal/clawvisorcli/cmd_agent_lite.go), NOT a bare OPENAI_API_KEY bearer.
// codexEnv here approximates that with OPENAI_BASE_URL/OPENAI_API_KEY and is
// UNVERIFIED (no OpenAI key was available to smoke it); prefer wiring these
// scenarios through the CLI when validating Codex. See e2e/README.md.
var codexReady = regexp.MustCompile(`(?i)Codex|for shortcuts|send a message|esc to`)

func codexInteractive() string {
	return "codex --model " + codexAgentModel
}

// TestRealCodexAttribution runs a trivial prompt and asserts a proxy-lite
// session attributed to the registered agent ID appears server-side.
func TestRealCodexAttribution(t *testing.T) {
	requireRealClient(t)
	key := requireKey(t, "OPENAI_API_KEY")
	requireCommand(t, "codex")

	l := bootLite(t, "openai", key)
	sess := newSession(t, "codex-attr")
	sess.launch(codexEnv(t, l), codexInteractive())

	sess.waitForPane(codexReady, 30*time.Second)
	sess.sendKeys("Reply with the single word: pong")

	if !l.waitForAgentAudit(t, l.agent.ID, 90*time.Second) {
		t.Fatalf("no proxy-lite session attributed to agent %s after prompt.\n=== PANE ===\n%s", l.agent.ID, sess.capture())
	}
}

// TestRealCodexKeyInjection proves the client holds no provider key: with only
// its cvis_ token it still reaches upstream (vault injection), and after the
// vault entry is deleted a fresh prompt fails with an auth error in the pane.
func TestRealCodexKeyInjection(t *testing.T) {
	requireRealClient(t)
	key := requireKey(t, "OPENAI_API_KEY")
	requireCommand(t, "codex")

	l := bootLite(t, "openai", key)

	ok := newSession(t, "codex-inject-ok")
	ok.launch(codexEnv(t, l), codexInteractive())
	ok.waitForPane(codexReady, 30*time.Second)
	ok.sendKeys("Reply with the single word: pong")
	if !l.waitForAgentAudit(t, l.agent.ID, 90*time.Second) {
		t.Fatalf("prompt did not reach upstream via vault injection.\n=== PANE ===\n%s", ok.capture())
	}

	l.deleteVaultKey(t, "openai")
	bad := newSession(t, "codex-inject-bad")
	bad.launch(codexEnv(t, l), codexInteractive())
	bad.waitForPane(codexReady, 30*time.Second)
	bad.sendKeys("Reply with the single word: pong")
	bad.waitForPane(regexp.MustCompile(`(?i)(auth|api key|credential|401|unauthor|no .*key)`), 60*time.Second)
}
