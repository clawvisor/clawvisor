package policies_test

import (
	"context"
	"strings"
	"testing"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/pipeline"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/policies"
)

// TestIntegration_RealPreprocessChain validates the Phase 3 milestone:
// the four migrated preprocess policies (anthropic_sanitize,
// inbound_sanitize, synthetic_history_strip, secret_history_strip)
// compose correctly when run through the real Pipeline.RunPre with
// the real eagerRequestMutator and a real ReadOnlyRequest impl.
//
// This is the abstraction's first end-to-end load-bearing test: a body
// that triggers ALL FOUR policies' mutation paths is fed through the
// chain, and the final result is verified to have every per-policy
// audit flag set and every artifact removed.
func TestIntegration_RealPreprocessChain(t *testing.T) {
	// Build a body that hits every migrated policy's mutation path:
	//   1. Empty text block (triggers anthropic_sanitize)
	//   2. Assistant tool_use with cv-nonce + X-Clawvisor-Caller +
	//      localhost URL (triggers inbound_sanitize)
	//   3. Assistant turn with InlineApprovalSubstitutedPromptMarker
	//      (triggers synthetic_history_strip)
	//   4. Assistant turn with SecretDecisionIDMarker
	//      (triggers secret_history_strip)
	marker := llmproxy.InlineApprovalSubstitutedPromptMarker
	secretMarker := llmproxy.SecretDecisionIDMarker
	body := `{"model":"claude-sonnet-4","messages":[` +
		// Empty text block — anthropic_sanitize drops this.
		`{"role":"user","content":[{"type":"text","text":""},{"type":"text","text":"Can you delete it?"}]},` +
		// Rewritten tool_use — inbound_sanitize reverts this.
		`{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"cmd":"curl -H 'X-Clawvisor-Caller: cv-nonce-abc123' http://localhost:25297/api/proxy/api.github.com/repos/x/y/issues"}}]},` +
		// Inline approval prompt + bare reply — synthetic_history_strip drops.
		`{"role":"assistant","content":[{"type":"text","text":"` + marker + ` cv-approve-1"}]},` +
		`{"role":"user","content":[{"type":"text","text":"y"}]},` +
		// Secret-decision marker — secret_history_strip drops.
		`{"role":"assistant","content":[{"type":"text","text":"` + secretMarker + `xyz789]"}]},` +
		`{"role":"user","content":[{"type":"text","text":"discard"}]}` +
		`]}`

	chain := []pipeline.RequestPolicy{
		policies.NewAnthropicSanitize(),
		policies.NewInboundSanitize("http://localhost:25297/api/proxy", "http://localhost:25297"),
		policies.NewSyntheticHistoryStrip(),
		policies.NewSecretHistoryStrip(),
	}

	req := &stubReadOnlyRequest{
		provider: conversation.ProviderAnthropic,
		rawBody:  []byte(body),
	}

	result, err := pipeline.RunPre(context.Background(), req, chain)
	if err != nil {
		t.Fatalf("RunPre: %v", err)
	}

	final := string(result.FinalBody)

	// Each policy's mutation must have landed in the final body.
	if strings.Contains(final, `"text":""`) {
		t.Errorf("anthropic_sanitize didn't fire: empty text block remains\n%s", final)
	}
	if strings.Contains(final, "cv-nonce-abc123") {
		t.Errorf("inbound_sanitize didn't fire: cv-nonce remains\n%s", final)
	}
	if strings.Contains(final, marker) {
		t.Errorf("synthetic_history_strip didn't fire: inline approval marker remains\n%s", final)
	}
	if strings.Contains(final, secretMarker) {
		t.Errorf("secret_history_strip didn't fire: secret-decision marker remains\n%s", final)
	}

	// Each policy's audit flag must be set.
	wantAuditFlags := []string{
		"anthropic_empty_text_sanitized",
		"inbound_history_sanitized",
		"synthetic_approval_history_stripped",
		"secret_history_stripped",
	}
	for _, flag := range wantAuditFlags {
		if got := result.AuditFields[flag]; got != true {
			t.Errorf("audit flag %q = %v, want true (full audit: %+v)", flag, got, result.AuditFields)
		}
	}

	// The original user prompt must survive.
	if !strings.Contains(final, "Can you delete it?") {
		t.Errorf("legitimate user content lost from final body:\n%s", final)
	}

	// All four policies should appear in the verdict trail (each ran).
	if len(result.Verdicts) != 4 {
		t.Errorf("expected 4 verdicts, got %d: %+v", len(result.Verdicts), result.Verdicts)
	}
	// And no Deny / ShortCircuit fired.
	if result.DenyReason != "" {
		t.Errorf("unexpected deny: %s by %s", result.DenyReason, result.DeniedBy)
	}
	if result.ShortCircuit != nil {
		t.Errorf("unexpected short-circuit: %+v", result.ShortCircuit)
	}
}

// TestIntegration_NonAnthropicSkipsAnthropicPolicy verifies that the
// chain handles provider variance correctly: an OpenAI request runs
// the same chain but anthropic_sanitize emits OutcomeSkip (no mutation,
// no audit field), while the other three policies still run.
func TestIntegration_NonAnthropicSkipsAnthropicPolicy(t *testing.T) {
	// Clean OpenAI body — no mutations expected from any policy.
	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hello"}]}`

	chain := []pipeline.RequestPolicy{
		policies.NewAnthropicSanitize(),
		policies.NewInboundSanitize("", ""),
		policies.NewSyntheticHistoryStrip(),
		policies.NewSecretHistoryStrip(),
	}

	req := &stubReadOnlyRequest{
		provider: conversation.ProviderOpenAI,
		rawBody:  []byte(body),
	}

	result, err := pipeline.RunPre(context.Background(), req, chain)
	if err != nil {
		t.Fatalf("RunPre: %v", err)
	}

	if string(result.FinalBody) != body {
		t.Errorf("clean OpenAI body got mutated\n--- want ---\n%s\n--- got ---\n%s", body, result.FinalBody)
	}
	// No audit flags should have been set for a clean body.
	for _, flag := range []string{
		"anthropic_empty_text_sanitized",
		"inbound_history_sanitized",
		"synthetic_approval_history_stripped",
		"secret_history_stripped",
	} {
		if _, ok := result.AuditFields[flag]; ok {
			t.Errorf("audit flag %q unexpectedly set on clean body", flag)
		}
	}
	// anthropic_sanitize must specifically be the one that returned Skip.
	for _, v := range result.Verdicts {
		if v.Name == "anthropic_sanitize" {
			if v.Verdict.Outcome != pipeline.OutcomeSkip {
				t.Errorf("anthropic_sanitize on OpenAI request: Outcome = %q, want Skip", v.Verdict.Outcome)
			}
		}
	}
}
