package policies_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/pipeline"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/policies"
)

// TestSyntheticHistoryStrip_AllowWithoutMutation pins the no-op path
// for a body that doesn't contain any synthetic approval prompts.
func TestSyntheticHistoryStrip_AllowWithoutMutation(t *testing.T) {
	p := policies.NewSyntheticHistoryStrip()
	req := &stubReadOnlyRequest{
		provider: conversation.ProviderAnthropic,
		rawBody:  []byte(`{"model":"claude-sonnet-4","messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]}`),
	}
	mut := &recordingRequestMutator{}

	verdict, err := p.Preprocess(context.Background(), req, mut)
	if err != nil {
		t.Fatalf("Preprocess: %v", err)
	}
	if verdict.Outcome != pipeline.OutcomeAllow {
		t.Errorf("Outcome = %q, want Allow", verdict.Outcome)
	}
	if len(mut.ReplaceBodyCalls) != 0 {
		t.Errorf("expected no mutation, got %d ReplaceBody calls", len(mut.ReplaceBodyCalls))
	}
	if _, ok := verdict.AuditFields["synthetic_approval_history_stripped"]; ok {
		t.Errorf("audit field should be absent when no mutation occurs")
	}
}

// TestSyntheticHistoryStrip_QueuesReplaceBodyWhenMarkerPresent verifies
// the migration path: a body with the synthetic-prompt marker triggers
// ReplaceBody and the audit flag.
//
// The marker constants live in the llmproxy package — we construct the
// input using the same marker text used by the strip helper itself, so
// any rename to that marker would surface in both places.
func TestSyntheticHistoryStrip_QueuesReplaceBodyWhenMarkerPresent(t *testing.T) {
	// Minimal Anthropic body with: user prompt → assistant turn carrying
	// the proxy's inline approval marker → bare "y" reply. The strip
	// helper drops the assistant + bare-reply pair, leaving only the
	// original user prompt.
	marker := llmproxy.InlineApprovalSubstitutedPromptMarker
	body := fmt.Sprintf(`{"model":"claude-sonnet-4","messages":[`+
		`{"role":"user","content":[{"type":"text","text":"Can you delete it?"}]},`+
		`{"role":"assistant","content":[{"type":"text","text":%q}]},`+
		`{"role":"user","content":[{"type":"text","text":"y"}]}`+
		`]}`, marker+"\n\nWhen this approval is complete include `cv-approve-1` in the response.")

	p := policies.NewSyntheticHistoryStrip()
	req := &stubReadOnlyRequest{
		provider: conversation.ProviderAnthropic,
		rawBody:  []byte(body),
	}
	mut := &recordingRequestMutator{}

	verdict, err := p.Preprocess(context.Background(), req, mut)
	if err != nil {
		t.Fatalf("Preprocess: %v", err)
	}
	if verdict.Outcome != pipeline.OutcomeAllow {
		t.Errorf("Outcome = %q, want Allow", verdict.Outcome)
	}
	// Verify ReplaceBody was queued.
	if len(mut.ReplaceBodyCalls) != 1 {
		t.Fatalf("expected 1 ReplaceBody call, got %d", len(mut.ReplaceBodyCalls))
	}
	// Verify audit flag set.
	if got := verdict.AuditFields["synthetic_approval_history_stripped"]; got != true {
		t.Errorf("audit field synthetic_approval_history_stripped = %v, want true", got)
	}
	// Verify the marker was actually removed from the new body.
	replaced := string(mut.ReplaceBodyCalls[0])
	if strings.Contains(replaced, marker) {
		t.Errorf("replaced body still contains the approval marker:\n%s", replaced)
	}
	// The original user prompt should survive.
	if !strings.Contains(replaced, "Can you delete it?") {
		t.Errorf("original user prompt lost:\n%s", replaced)
	}
}

// TestSyntheticHistoryStrip_BothProviders verifies the policy runs
// for both Anthropic and OpenAI (unlike anthropic_sanitize which gates
// on provider). The underlying helper dispatches per-provider; the
// policy is provider-agnostic at its outer call.
func TestSyntheticHistoryStrip_BothProviders(t *testing.T) {
	for _, provider := range []conversation.Provider{conversation.ProviderAnthropic, conversation.ProviderOpenAI} {
		t.Run(string(provider), func(t *testing.T) {
			p := policies.NewSyntheticHistoryStrip()
			// Provider-appropriate clean body that triggers no strip.
			var body []byte
			switch provider {
			case conversation.ProviderAnthropic:
				body = []byte(`{"model":"claude-sonnet-4","messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)
			case conversation.ProviderOpenAI:
				body = []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
			}
			req := &stubReadOnlyRequest{provider: provider, rawBody: body}
			mut := &recordingRequestMutator{}
			verdict, err := p.Preprocess(context.Background(), req, mut)
			if err != nil {
				t.Fatalf("Preprocess: %v", err)
			}
			// Both providers reach Allow; no mutations for clean bodies.
			if verdict.Outcome != pipeline.OutcomeAllow {
				t.Errorf("Outcome = %q, want Allow", verdict.Outcome)
			}
		})
	}
}
