//go:build realclient

package realclient

import (
	"regexp"
	"testing"
	"time"
)

// The Claude Code realclient scenarios. Each boots a live proxy-lite server,
// stores the real Anthropic key in the vault, hands the client ONLY the cvis_
// agent token, and asserts server-side records. They are gated on
// CLAWVISOR_REALCLIENT=1 + ANTHROPIC_API_KEY and skip cleanly otherwise.
//
// Cost control: the cheapest model, single-shot prompts, and a 30m overall
// timeout (see realclient.yml). We never assert on model output text.

const claudeAgentModel = "claude-3-5-haiku-latest"

// claudeReady matches a stable Claude Code TUI landmark. It deliberately avoids
// loose tokens like ">" (which match the launching shell prompt and cause a
// premature match); it keys on the banner + input-affordance strings the TUI
// prints once it is up. NB: these strings track a weekly-shipping TUI and may
// need per-version tuning — the lane is advisory, not a merge gate, for exactly
// this reason (see e2e/README.md).
var claudeReady = regexp.MustCompile(`(?i)Welcome to Claude Code|for shortcuts|esc to interrupt|bypass permissions`)

// claudeInteractive returns the tmux command that launches the Claude Code TUI.
// --model pins the cheapest model. --dangerously-skip-permissions suppresses
// Claude Code's OWN interactive tool-permission prompts (NOT Clawvisor's) so a
// tool_use flows straight to proxy-lite, where Clawvisor's task-approval policy
// is what actually gates it — that's the behavior under test.
func claudeInteractive(l *liteServer) string {
	return "claude --model " + claudeAgentModel + " --dangerously-skip-permissions"
}

// TestRealClaudeAttribution runs a trivial prompt and asserts a proxy-lite
// session attributed to the registered agent ID appears server-side.
func TestRealClaudeAttribution(t *testing.T) {
	requireRealClient(t)
	key := requireKey(t, "ANTHROPIC_API_KEY")
	requireCommand(t, "claude")

	l := bootLite(t, "anthropic", key)
	sess := newSession(t, "cc-attr")
	sess.launch(claudeEnv(t, l), claudeInteractive(l))

	// Wait for the TUI to be ready, then send a trivial single-turn prompt.
	sess.waitForPane(claudeReady, 30*time.Second)
	sess.sendKeys("Reply with the single word: pong")

	// The definitive signal is server-side: an audit row attributed to this agent.
	if !l.waitForAgentAudit(t, l.agent.ID, 90*time.Second) {
		t.Fatalf("no proxy-lite session attributed to agent %s after prompt.\n=== PANE ===\n%s", l.agent.ID, sess.capture())
	}
}

// TestRealClaudeKeyInjection proves the client never holds a provider key: with
// only its cvis_ token, a prompt still succeeds (vault injection worked). The
// negative check deletes the vault entry and asserts a fresh prompt fails with
// an auth error surfaced in the pane.
func TestRealClaudeKeyInjection(t *testing.T) {
	requireRealClient(t)
	key := requireKey(t, "ANTHROPIC_API_KEY")
	requireCommand(t, "claude")

	l := bootLite(t, "anthropic", key)

	// Positive: prompt succeeds via injected vault key.
	ok := newSession(t, "cc-inject-ok")
	ok.launch(claudeEnv(t, l), claudeInteractive(l))
	ok.waitForPane(claudeReady, 30*time.Second)
	ok.sendKeys("Reply with the single word: pong")
	if !l.waitForAgentAudit(t, l.agent.ID, 90*time.Second) {
		t.Fatalf("prompt did not reach upstream via vault injection.\n=== PANE ===\n%s", ok.capture())
	}

	// Negative: remove the vault key, new session, prompt should fail with auth.
	l.deleteVaultKey(t, "anthropic")
	bad := newSession(t, "cc-inject-bad")
	bad.launch(claudeEnv(t, l), claudeInteractive(l))
	bad.waitForPane(claudeReady, 30*time.Second)
	bad.sendKeys("Reply with the single word: pong")
	// With no vaulted key and an empty client key, the proxy has nothing to
	// inject → an authentication/credential error surfaces in the pane.
	bad.waitForPane(regexp.MustCompile(`(?i)(auth|api key|credential|401|unauthor|no .*key)`), 60*time.Second)
}

// TestRealClaudeToolUseHoldApprove drives a gated tool call, approves the hold
// via the API, and asserts the client continues.
func TestRealClaudeToolUseHoldApprove(t *testing.T) {
	requireRealClient(t)
	key := requireKey(t, "ANTHROPIC_API_KEY")
	requireCommand(t, "claude")

	l := bootLite(t, "anthropic", key)

	sess := newSession(t, "cc-hold-approve")
	sess.launch(claudeEnv(t, l), claudeInteractive(l))
	sess.waitForPane(claudeReady, 30*time.Second)
	// A prompt that requires a gated tool (shell command). The agent has no
	// active task, so proxy-lite's task-approval policy parks the bare tool_use
	// for human approval (see internal/api/handlers/llm_endpoint.go's
	// PendingApprovals cache + policies.NewTaskApprovalReply) — no server-side
	// seeding endpoint is needed.
	sess.sendKeys("Run the shell command `echo clawvisor-realclient` and tell me the output.")

	requestID := l.waitForPendingApproval(t, 90*time.Second)
	if requestID == "" {
		t.Fatalf("no pending approval appeared for gated tool call.\n=== PANE ===\n%s", sess.capture())
	}
	if code := l.resolveApproval(t, requestID, true); code >= 300 {
		t.Fatalf("resolve approve: status %d", code)
	}
	// Client continues after approval — the pane advances past the hold. We
	// assert on a stable "continuing/completed" indicator, not model text.
	sess.waitForPane(regexp.MustCompile(`(?i)(clawvisor-realclient|done|output|result)`), 90*time.Second)
}

// TestRealClaudeToolUseHoldDeny drives the same gated tool call and denies it;
// the denial is surfaced to the model and recorded in the audit trail.
func TestRealClaudeToolUseHoldDeny(t *testing.T) {
	requireRealClient(t)
	key := requireKey(t, "ANTHROPIC_API_KEY")
	requireCommand(t, "claude")

	l := bootLite(t, "anthropic", key)

	sess := newSession(t, "cc-hold-deny")
	sess.launch(claudeEnv(t, l), claudeInteractive(l))
	sess.waitForPane(claudeReady, 30*time.Second)
	sess.sendKeys("Run the shell command `echo clawvisor-realclient` and tell me the output.")

	requestID := l.waitForPendingApproval(t, 90*time.Second)
	if requestID == "" {
		t.Fatalf("no pending approval appeared for gated tool call.\n=== PANE ===\n%s", sess.capture())
	}
	if code := l.resolveApproval(t, requestID, false); code >= 300 {
		t.Fatalf("resolve deny: status %d", code)
	}
	// The denial is surfaced to the model — the pane shows a denied/blocked
	// indicator rather than the command output.
	sess.waitForPane(regexp.MustCompile(`(?i)(deni|declin|block|not allowed|reject)`), 90*time.Second)
}
