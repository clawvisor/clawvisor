package policies_test

import (
	"context"
	"testing"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/pipeline"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/policies"
)

// TestAgentNoticeResponse_SkipsWhenEmpty pins the gate: the handler
// passes "" when the request isn't first-turn or no notice is wanted;
// the policy must Skip with no mutation.
func TestAgentNoticeResponse_SkipsWhenEmpty(t *testing.T) {
	p := policies.NewAgentNoticeResponse("")
	res := &stubReadOnlyResponse{provider: conversation.ProviderAnthropic}
	mut := &recordingResponseMutator{}

	verdict, err := p.Postprocess(context.Background(), res, mut)
	if err != nil {
		t.Fatalf("Postprocess: %v", err)
	}
	if verdict.Outcome != pipeline.OutcomeSkip {
		t.Errorf("Outcome = %q, want Skip", verdict.Outcome)
	}
	if len(mut.PrependAssistantTextCalls) != 0 {
		t.Errorf("expected no PrependAssistantText calls, got %d", len(mut.PrependAssistantTextCalls))
	}
}

// TestAgentNoticeResponse_PrependsAndAudits verifies the migration
// path: non-empty notice text triggers PrependAssistantText and the
// agent_notice_prepended audit flag.
func TestAgentNoticeResponse_PrependsAndAudits(t *testing.T) {
	const notice = "[Clawvisor] Routing to claude-code..."
	p := policies.NewAgentNoticeResponse(notice)
	res := &stubReadOnlyResponse{provider: conversation.ProviderAnthropic}
	mut := &recordingResponseMutator{}

	verdict, err := p.Postprocess(context.Background(), res, mut)
	if err != nil {
		t.Fatalf("Postprocess: %v", err)
	}
	if verdict.Outcome != pipeline.OutcomeAllow {
		t.Errorf("Outcome = %q, want Allow", verdict.Outcome)
	}
	if len(mut.PrependAssistantTextCalls) != 1 {
		t.Fatalf("expected 1 PrependAssistantText call, got %d", len(mut.PrependAssistantTextCalls))
	}
	if mut.PrependAssistantTextCalls[0] != notice {
		t.Errorf("queued text %q, want %q", mut.PrependAssistantTextCalls[0], notice)
	}
	if got := verdict.AuditFields["agent_notice_prepended"]; got != true {
		t.Errorf("audit field agent_notice_prepended = %v, want true", got)
	}
}
