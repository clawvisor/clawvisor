package policies_test

import (
	"context"
	"testing"
	"time"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/pipeline"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/policies"
)

func inlineTaskAugmenter(store llmproxy.InlineApprovalOutcomeStore) policies.InlineTaskHistoryAugmenter {
	if store == nil {
		return nil
	}
	return func(_ context.Context, req policies.InlineTaskHistoryAugmentRequest) (policies.InlineTaskHistoryAugmentResult, error) {
		body, modified, err := llmproxy.AugmentApprovedInlineTasksInHistory(req.Body, req.Provider, store, req.UserID, req.AgentID)
		return policies.InlineTaskHistoryAugmentResult{Body: body, Modified: modified}, err
	}
}

// TestInlineTaskAugment_SkipsWithoutUserID pins the gate: a request
// without a UserID/AgentID can't scope the outcome store, so the
// policy emits Skip rather than risking a nil/empty lookup.
func TestInlineTaskAugment_SkipsWithoutUserID(t *testing.T) {
	store := llmproxy.NewMemoryInlineApprovalOutcomeStore(time.Hour)
	p := policies.NewInlineTaskAugment(inlineTaskAugmenter(store))
	req := &stubReadOnlyRequest{
		provider: conversation.ProviderAnthropic,
		rawBody:  []byte(`{"model":"claude-sonnet-4","messages":[]}`),
		// no userID / agentID
	}
	mut := &recordingRequestMutator{}

	verdict, err := p.Preprocess(context.Background(), req, mut)
	if err != nil {
		t.Fatalf("Preprocess: %v", err)
	}
	if verdict.Outcome != pipeline.OutcomeSkip {
		t.Errorf("Outcome = %q, want Skip", verdict.Outcome)
	}
	if len(mut.ReplaceBodyCalls) != 0 {
		t.Errorf("expected no mutation without scope, got %d", len(mut.ReplaceBodyCalls))
	}
}

// TestInlineTaskAugment_SkipsWithNilStore guards against a nil store
// dependency (constructor invariant).
func TestInlineTaskAugment_SkipsWithNilStore(t *testing.T) {
	p := policies.NewInlineTaskAugment(nil)
	req := &stubReadOnlyRequest{
		provider: conversation.ProviderAnthropic,
		rawBody:  []byte(`{"model":"claude-sonnet-4","messages":[]}`),
		userID:   "u1",
		agentID:  "a1",
	}
	mut := &recordingRequestMutator{}

	verdict, err := p.Preprocess(context.Background(), req, mut)
	if err != nil {
		t.Fatalf("Preprocess: %v", err)
	}
	if verdict.Outcome != pipeline.OutcomeSkip {
		t.Errorf("Outcome = %q, want Skip", verdict.Outcome)
	}
}

// TestInlineTaskAugment_AllowWithoutMutation verifies the no-op path:
// a body whose history doesn't trigger augmentation passes through
// unchanged.
func TestInlineTaskAugment_AllowWithoutMutation(t *testing.T) {
	store := llmproxy.NewMemoryInlineApprovalOutcomeStore(time.Hour)
	p := policies.NewInlineTaskAugment(inlineTaskAugmenter(store))
	req := &stubReadOnlyRequest{
		provider: conversation.ProviderAnthropic,
		rawBody:  []byte(`{"model":"claude-sonnet-4","messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`),
		userID:   "u1",
		agentID:  "a1",
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
		t.Errorf("expected no mutation, got %d", len(mut.ReplaceBodyCalls))
	}
}
