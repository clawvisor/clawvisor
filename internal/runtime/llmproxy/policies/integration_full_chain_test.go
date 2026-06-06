package policies_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/pipeline"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/policies"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// TestIntegration_FullPreprocessChain runs the full set of migrated
// preprocess policies — including the stateful ones — through
// Pipeline.RunPre.
//
// The chain order mirrors the legacy handler's call order (see §4 of
// .context/llmproxy-refactor-plan.md). Each policy emits Skip /
// Allow against an unprovocative request so the chain runs to
// completion and we can verify aggregated audit fields and that
// no policy errors.
func TestIntegration_FullPreprocessChain(t *testing.T) {
	// Per-request state.
	cache := llmproxy.NewMemoryPendingApprovalCache(time.Hour)
	outcomes := llmproxy.NewMemoryInlineApprovalOutcomeStore(time.Hour)
	agent := &store.Agent{ID: "a1", UserID: "u1"}

	// Compose the full chain. Order mirrors the handler:
	//   1. anthropic_sanitize
	//   2. inbound_sanitize
	//   3. secret_history_strip
	//   4. task_approval_reply
	//   5. inline_task_intercept
	//   6. inline_task_augment
	//   7. control_notice
	//   8. synthetic_history_strip
	chain := []pipeline.RequestPolicy{
		policies.NewAnthropicSanitize(),
		policies.NewInboundSanitize("http://localhost:25297/api/proxy", "http://localhost:25297"),
		policies.NewSecretHistoryStrip(),
		policies.NewTaskApprovalReply(cache, agent, noopTaskApprovalRewriter),
		policies.NewInlineTaskIntercept(cache, agent, "req-1", noopInlineTaskApprovalRewriter),
		policies.NewInlineTaskAugment(inlineTaskAugmenter(outcomes)),
		policies.NewControlNotice("http://localhost:25297", oneToolAvailable, noopToolRules),
		policies.NewSyntheticHistoryStrip(),
	}

	// Body declares one tool — exercises the control_notice gate.
	body := []byte(`{"model":"claude-sonnet-4","tools":[{"name":"Bash"}],"messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]}`)
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(string(body)))

	req := &stubReadOnlyRequest{
		provider:        conversation.ProviderAnthropic,
		rawBody:         body,
		userID:          "u1",
		agentID:         "a1",
		httpReqOverride: httpReq,
	}

	result, err := pipeline.RunPre(context.Background(), req, chain)
	if err != nil {
		t.Fatalf("RunPre: %v", err)
	}

	// 8 policies; each appears in the verdict trail.
	if len(result.Verdicts) != 8 {
		t.Errorf("expected 8 verdicts, got %d", len(result.Verdicts))
	}

	// No Deny / no ShortCircuit on this benign request.
	if result.DenyReason != "" {
		t.Errorf("unexpected deny: %s by %s", result.DenyReason, result.DeniedBy)
	}
	if result.ShortCircuit != nil {
		t.Errorf("unexpected short-circuit: %+v", result.ShortCircuit)
	}

	// Only control_notice should have mutated this body (no empty text
	// blocks, no rewritten artifacts, no inline approval markers, etc.).
	if got := result.AuditParams["control_notice_injected"]; got != true {
		t.Errorf("expected control_notice_injected, got %v\n%+v", got, result.AuditParams)
	}

	// And the final body must include the notice text.
	if !strings.Contains(string(result.FinalBody), "Clawvisor proxy-lite control plane") {
		t.Errorf("notice not in final body:\n%s", result.FinalBody)
	}

	// No other audit flags should be set — the chain ran cleanly.
	unwantedFlags := []string{
		"anthropic_empty_text_sanitized",
		"inbound_history_sanitized",
		"secret_history_stripped",
		"approval_task_rewritten",
		"inline_task_approval_rewritten",
		"inline_task_history_augmented",
		"synthetic_approval_history_stripped",
	}
	for _, flag := range unwantedFlags {
		if _, ok := result.AuditParams[flag]; ok {
			t.Errorf("unexpected audit flag set on benign request: %q (full audit: %+v)", flag, result.AuditParams)
		}
	}
}

// TestIntegration_FullChainStopsOnDeny verifies that a Deny from any
// policy halts the chain. Here we feed a malformed body that
// anthropic_sanitize will Deny; the remaining 7 policies must not run.
func TestIntegration_FullChainStopsOnDeny(t *testing.T) {
	cache := llmproxy.NewMemoryPendingApprovalCache(time.Hour)
	agent := &store.Agent{ID: "a1", UserID: "u1"}

	chain := []pipeline.RequestPolicy{
		policies.NewAnthropicSanitize(),
		policies.NewInboundSanitize("", ""),
		policies.NewTaskApprovalReply(cache, agent, noopTaskApprovalRewriter),
		policies.NewInlineTaskIntercept(cache, agent, "req-1", noopInlineTaskApprovalRewriter),
	}

	httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))

	req := &stubReadOnlyRequest{
		provider:        conversation.ProviderAnthropic,
		rawBody:         []byte(`{not valid json`),
		userID:          "u1",
		agentID:         "a1",
		httpReqOverride: httpReq,
	}

	result, err := pipeline.RunPre(context.Background(), req, chain)
	if err != nil {
		t.Fatalf("RunPre: %v", err)
	}
	if result.DenyReason == "" {
		t.Errorf("expected DenyReason populated")
	}
	if result.DeniedBy != "anthropic_sanitize" {
		t.Errorf("DeniedBy = %q, want anthropic_sanitize", result.DeniedBy)
	}
	// Only anthropic_sanitize ran; the remaining 3 policies didn't.
	if len(result.Verdicts) != 1 {
		t.Errorf("expected 1 verdict (anthropic_sanitize denied), got %d", len(result.Verdicts))
	}
}
