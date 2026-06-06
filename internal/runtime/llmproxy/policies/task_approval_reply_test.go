package policies_test

import (
	"context"
	"testing"
	"time"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/pipeline"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/policies"
	"github.com/clawvisor/clawvisor/pkg/store"
)

func noopTaskApprovalRewriter(_ context.Context, req policies.TaskApprovalReplyRequest) (policies.TaskApprovalReplyResult, error) {
	return policies.TaskApprovalReplyResult{Body: req.Body}, nil
}

// TestTaskApprovalReply_SkipsWithoutCache pins the gate: nil cache
// → Skip.
func TestTaskApprovalReply_SkipsWithoutCache(t *testing.T) {
	p := policies.NewTaskApprovalReply(nil, &store.Agent{ID: "a1", UserID: "u1"}, noopTaskApprovalRewriter)
	req := &stubReadOnlyRequest{
		provider: conversation.ProviderAnthropic,
		rawBody:  []byte(`{"model":"claude-sonnet-4","messages":[{"role":"user","content":"task"}]}`),
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

// TestTaskApprovalReply_SkipsWithoutAgent pins the same gate for the
// agent-missing branch.
func TestTaskApprovalReply_SkipsWithoutAgent(t *testing.T) {
	cache := llmproxy.NewMemoryPendingApprovalCache(time.Hour)
	p := policies.NewTaskApprovalReply(cache, nil, noopTaskApprovalRewriter)
	req := &stubReadOnlyRequest{
		provider: conversation.ProviderAnthropic,
		rawBody:  []byte(`{"model":"claude-sonnet-4","messages":[{"role":"user","content":"task"}]}`),
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

// TestTaskApprovalReply_AllowWithoutMutation pins the no-op path:
// a body without a "task" reply verb falls through with no rewrite.
func TestTaskApprovalReply_AllowWithoutMutation(t *testing.T) {
	cache := llmproxy.NewMemoryPendingApprovalCache(time.Hour)
	p := policies.NewTaskApprovalReply(cache, &store.Agent{ID: "a1", UserID: "u1"}, noopTaskApprovalRewriter)
	req := &stubReadOnlyRequest{
		provider: conversation.ProviderAnthropic,
		rawBody:  []byte(`{"model":"claude-sonnet-4","messages":[{"role":"user","content":"hello"}]}`),
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
		t.Errorf("expected no mutation, got %d ReplaceBody calls", len(mut.ReplaceBodyCalls))
	}
	if _, ok := verdict.AuditParams["approval_task_rewritten"]; ok {
		t.Errorf("audit field should be absent when no rewrite occurs")
	}
}
