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

// TestInlineTaskIntercept_SkipsWithoutCacheOrAgent pins the nil-checks.
func TestInlineTaskIntercept_SkipsWithoutCacheOrAgent(t *testing.T) {
	cases := []struct {
		name  string
		cache policies.PendingApprovalCacheView
		agent *store.Agent
	}{
		{"no cache", nil, &store.Agent{ID: "a", UserID: "u"}},
		{"no agent", llmproxy.NewMemoryPendingApprovalCache(time.Hour), nil},
		{"both nil", nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := policies.NewInlineTaskIntercept(tc.cache, tc.agent, nil, nil, "req-1", nil, nil)
			req := &stubReadOnlyRequest{
				provider: conversation.ProviderAnthropic,
				rawBody:  []byte(`{"model":"claude-sonnet-4","messages":[{"role":"user","content":"approve"}]}`),
			}
			mut := &recordingRequestMutator{}
			verdict, err := p.Preprocess(context.Background(), req, mut)
			if err != nil {
				t.Fatalf("Preprocess: %v", err)
			}
			if verdict.Outcome != pipeline.OutcomeSkip {
				t.Errorf("Outcome = %q, want Skip", verdict.Outcome)
			}
		})
	}
}

// TestInlineTaskIntercept_AllowWithoutMutation pins the no-op path:
// a body without any pending inline-task hold falls through with no
// rewrite.
func TestInlineTaskIntercept_AllowWithoutMutation(t *testing.T) {
	cache := llmproxy.NewMemoryPendingApprovalCache(time.Hour)
	agent := &store.Agent{ID: "a1", UserID: "u1"}
	p := policies.NewInlineTaskIntercept(cache, agent, nil, nil, "req-1", nil, nil)

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
	if _, ok := verdict.AuditParams["inline_task_approval_rewritten"]; ok {
		t.Errorf("audit fields should be absent when no rewrite occurs")
	}
}
