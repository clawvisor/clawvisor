package policies

import (
	"context"

	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/pipeline"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// TaskApprovalReply rewrites a user reply of "task" into the
// inline-task-definition flow. When the user replies "task" to a
// pending approval that supports inline task definition, the policy
// resolves the hold, starts the task-definition conversation, and
// rewrites the body so the LLM sees the task-definition prompt
// instead of a bare "task" string.
//
// This is a stateful policy: it takes both the pending-approval
// cache and the agent identity. The policy is constructed per
// request — the handler already has the resolved *store.Agent before
// any preprocess step runs.
//
// Underlying function: llmproxy.RewriteTaskApprovalReply (today
// called inline from llm_endpoint.go).
type TaskApprovalReply struct {
	cache PendingApprovalCacheView
	agent *store.Agent
}

// PendingApprovalCacheView narrows the cache interface to what this
// policy needs. The full llmproxy.PendingApprovalCache satisfies it.
type PendingApprovalCacheView = llmproxy.PendingApprovalCache

// NewTaskApprovalReply constructs the policy. agent and cache must
// be non-nil for the policy to act; nil values produce Skip rather
// than panicking.
func NewTaskApprovalReply(cache PendingApprovalCacheView, agent *store.Agent) *TaskApprovalReply {
	return &TaskApprovalReply{cache: cache, agent: agent}
}

// Name returns the audit-friendly policy identifier.
func (TaskApprovalReply) Name() string { return "task_approval_reply" }

// Preprocess attempts to rewrite a "task" reply into the inline-task-
// definition flow. Returns OutcomeAllow with no mutation when the
// reply isn't a task verb; OutcomeAllow with the body replaced when
// the rewrite fires; OutcomeDeny on a malformed reply.
func (p *TaskApprovalReply) Preprocess(ctx context.Context, req pipeline.ReadOnlyRequest, mut pipeline.RequestMutator) (pipeline.RequestVerdict, error) {
	if p.cache == nil || p.agent == nil {
		return pipeline.RequestVerdict{Outcome: pipeline.OutcomeSkip}, nil
	}

	rewrite, err := llmproxy.RewriteTaskApprovalReply(ctx, llmproxy.TaskReplyRewriteRequest{
		HTTPRequest:     req.HTTPRequest(),
		Provider:        req.Provider(),
		Body:            req.RawBody(),
		Agent:           p.agent,
		ConversationID:  req.ConversationID(),
		PendingApproval: p.cache,
	})
	if err != nil {
		return pipeline.RequestVerdict{
			Outcome: pipeline.OutcomeDeny,
			Reason:  err.Error(),
			AuditParams: map[string]any{
				"deny_outcome": "malformed_request",
			},
		}, nil
	}
	if !rewrite.Rewritten {
		return pipeline.RequestVerdict{Outcome: pipeline.OutcomeAllow}, nil
	}
	if err := mut.ReplaceBody(rewrite.Body); err != nil {
		return pipeline.RequestVerdict{}, err
	}
	return pipeline.RequestVerdict{
		Outcome: pipeline.OutcomeAllow,
		AuditParams: map[string]any{
			"approval_task_rewritten": true,
		},
	}, nil
}

var _ pipeline.RequestPolicy = (*TaskApprovalReply)(nil)
