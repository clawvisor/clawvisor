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

// TestSecretHistoryStrip_AllowWithoutMutation pins the no-op path:
// a body with no secret-decision marker passes through unchanged.
func TestSecretHistoryStrip_AllowWithoutMutation(t *testing.T) {
	p := policies.NewSecretHistoryStrip()
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
}

// TestSecretHistoryStrip_StripsAssistantMarkerTurn verifies the
// migration path: an assistant turn carrying the SecretDecisionIDMarker
// triggers ReplaceBody and the audit flag.
func TestSecretHistoryStrip_StripsAssistantMarkerTurn(t *testing.T) {
	marker := llmproxy.SecretDecisionIDMarker
	body := `{"model":"claude-sonnet-4","messages":[` +
		`{"role":"user","content":[{"type":"text","text":"Use this token: ghp_abc123"}]},` +
		`{"role":"assistant","content":[{"type":"text","text":"` + marker + `xyz789]"}]},` +
		`{"role":"user","content":[{"type":"text","text":"discard"}]}` +
		`]}`

	p := policies.NewSecretHistoryStrip()
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
	if len(mut.ReplaceBodyCalls) != 1 {
		t.Fatalf("expected 1 ReplaceBody call, got %d", len(mut.ReplaceBodyCalls))
	}
	if got := verdict.AuditFields["secret_history_stripped"]; got != true {
		t.Errorf("audit field secret_history_stripped = %v, want true", got)
	}

	replaced := string(mut.ReplaceBodyCalls[0])
	if strings.Contains(replaced, marker) {
		t.Errorf("secret-decision marker still in replaced body:\n%s", replaced)
	}
}
