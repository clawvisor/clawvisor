package policies

import (
	"context"

	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/pipeline"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// InlineTaskIntercept wraps llmproxy.RewriteInlineTaskApprovalReply
// behind RequestPolicy. The richest preprocess policy by dependency
// count — it's the inline-task-approval resolver that runs when the
// user replies "approve" / "deny" to a held inline-task creation.
//
// Per-request construction is required: the handler holds the resolved
// *store.Agent and the per-instance dependency graph (creator,
// audit emitter, outcome store, checkout store, pending-approval cache,
// request ID).
//
// The policy emits its audit fields via Verdict.AuditParams; the
// orchestrator merges them into the request's audit row. Today's
// handler stores the same fields (`inline_task_approval_rewritten`,
// `inline_task_outcome`, `inline_task_id`, etc.) inline on
// auditParams; the move preserves them.
type InlineTaskIntercept struct {
	cache     PendingApprovalCacheView
	creator   llmproxy.InlineTaskCreator
	outcomes  llmproxy.InlineApprovalOutcomeStore
	checkouts llmproxy.TaskCheckoutStore
	audit     *llmproxy.AuditEmitter
	requestID string
	agent     *store.Agent
}

// NewInlineTaskIntercept constructs the policy with all its
// per-request state. Any nil among (cache, agent) → Skip.
func NewInlineTaskIntercept(
	cache PendingApprovalCacheView,
	agent *store.Agent,
	creator llmproxy.InlineTaskCreator,
	audit *llmproxy.AuditEmitter,
	requestID string,
	outcomes llmproxy.InlineApprovalOutcomeStore,
	checkouts llmproxy.TaskCheckoutStore,
) *InlineTaskIntercept {
	return &InlineTaskIntercept{
		cache:     cache,
		creator:   creator,
		outcomes:  outcomes,
		checkouts: checkouts,
		audit:     audit,
		requestID: requestID,
		agent:     agent,
	}
}

// Name returns the audit-friendly policy identifier.
func (InlineTaskIntercept) Name() string { return "inline_task_intercept" }

// Preprocess attempts to resolve a pending inline-task hold from a
// user "approve" / "deny" reply.
//
// Outcomes:
//   - nil cache / nil agent → Skip
//   - Body unchanged (no rewrite) → Allow with no mutation
//   - Body rewritten on success → Allow with ReplaceBody + audit fields
//   - Body rewritten on deny / creator failure → Allow with ReplaceBody
//   - audit fields tagged with the failure outcome
//   - Underlying error → Deny (today's handler returns 400)
func (p *InlineTaskIntercept) Preprocess(ctx context.Context, req pipeline.ReadOnlyRequest, mut pipeline.RequestMutator) (pipeline.RequestVerdict, error) {
	if p.cache == nil || p.agent == nil {
		return pipeline.RequestVerdict{Outcome: pipeline.OutcomeSkip}, nil
	}

	rewrite, err := llmproxy.RewriteInlineTaskApprovalReply(ctx, llmproxy.InlineApprovalRewriteRequest{
		HTTPRequest:     req.HTTPRequest(),
		Provider:        req.Provider(),
		Body:            req.RawBody(),
		Agent:           p.agent,
		ConversationID:  req.ConversationID(),
		PendingApproval: p.cache,
		Creator:         p.creator,
		Audit:           p.audit,
		RequestID:       p.requestID,
		Outcomes:        p.outcomes,
		Checkouts:       p.checkouts,
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

	fields := map[string]any{
		"inline_task_approval_rewritten": true,
		"inline_task_outcome":            rewrite.Outcome,
	}
	if rewrite.TaskID != "" {
		fields["inline_task_id"] = rewrite.TaskID
	}
	if rewrite.CheckedOut {
		fields["inline_task_checked_out"] = true
	}
	if rewrite.Reason != "" {
		fields["inline_task_reason"] = rewrite.Reason
	}
	return pipeline.RequestVerdict{
		Outcome:     pipeline.OutcomeAllow,
		AuditParams: fields,
	}, nil
}

var _ pipeline.RequestPolicy = (*InlineTaskIntercept)(nil)
