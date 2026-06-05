package policies_test

import (
	"context"
	"testing"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/pipeline"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/policies"
)

// TestSecretRewrites_SkipsNilResolver pins the gate.
func TestSecretRewrites_SkipsNilResolver(t *testing.T) {
	p := policies.NewSecretRewrites(nil)
	req := &stubReadOnlyRequest{provider: conversation.ProviderAnthropic, rawBody: []byte(`{}`)}
	mut := &recordingRequestMutator{}
	verdict, err := p.Preprocess(context.Background(), req, mut)
	if err != nil {
		t.Fatalf("Preprocess: %v", err)
	}
	if verdict.Outcome != pipeline.OutcomeSkip {
		t.Errorf("Outcome = %q, want Skip", verdict.Outcome)
	}
}

// TestSecretRewrites_AllowWithoutRewrite pins the no-op path.
func TestSecretRewrites_AllowWithoutRewrite(t *testing.T) {
	resolver := func(_ context.Context, body []byte) ([]byte, bool) {
		return body, false
	}
	p := policies.NewSecretRewrites(resolver)
	req := &stubReadOnlyRequest{provider: conversation.ProviderAnthropic, rawBody: []byte(`{"clean":true}`)}
	mut := &recordingRequestMutator{}

	verdict, err := p.Preprocess(context.Background(), req, mut)
	if err != nil {
		t.Fatalf("Preprocess: %v", err)
	}
	if verdict.Outcome != pipeline.OutcomeAllow {
		t.Errorf("Outcome = %q, want Allow", verdict.Outcome)
	}
	if len(mut.ReplaceBodyCalls) != 0 {
		t.Errorf("expected no mutation, got %d", len(mut.ReplaceBodyCalls))
	}
}

// TestSecretRewrites_AppliesRewrite verifies the migration path:
// resolver returns modified=true → ReplaceBody + audit flag.
func TestSecretRewrites_AppliesRewrite(t *testing.T) {
	rewritten := []byte(`{"secret_redacted":true}`)
	resolver := func(_ context.Context, _ []byte) ([]byte, bool) {
		return rewritten, true
	}
	p := policies.NewSecretRewrites(resolver)
	req := &stubReadOnlyRequest{provider: conversation.ProviderAnthropic, rawBody: []byte(`{"secret":"ghp_xyz"}`)}
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
	if string(mut.ReplaceBodyCalls[0]) != string(rewritten) {
		t.Errorf("ReplaceBody arg = %q, want %q", mut.ReplaceBodyCalls[0], rewritten)
	}
	if v := verdict.AuditFields["secret_rewrites_applied"]; v != true {
		t.Errorf("secret_rewrites_applied = %v, want true", v)
	}
}
