package policies

import (
	"context"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/pipeline"
)

// InlineTaskAugment walks conversation history and re-injects
// approval-outcome context for inline tasks the user previously
// approved. Without this, the model sees only the user's bare
// "approve" reply on subsequent turns and may duplicate work
// (re-POST /api/control/tasks, re-emit the original tool_use).
//
// The underlying transform is byte-fidelity tested and handles both
// providers internally. Pure body transformation with a single
// state-store dependency (InlineApprovalOutcomes), threaded through
// the constructor.
type InlineTaskAugment struct {
	augment InlineTaskHistoryAugmenter
}

type InlineTaskHistoryAugmentRequest struct {
	Body     []byte
	Provider conversation.Provider
	UserID   string
	AgentID  string
}

type InlineTaskHistoryAugmentResult struct {
	Body     []byte
	Modified bool
}

type InlineTaskHistoryAugmenter func(ctx context.Context, req InlineTaskHistoryAugmentRequest) (InlineTaskHistoryAugmentResult, error)

// NewInlineTaskAugment constructs the policy with its outcome store
// dependency. The handler holds the canonical outcome store; passing
// it here keeps the policy testable in isolation against an in-memory
// store.
func NewInlineTaskAugment(augment InlineTaskHistoryAugmenter) *InlineTaskAugment {
	return &InlineTaskAugment{augment: augment}
}

// Name returns the audit-friendly policy identifier.
func (InlineTaskAugment) Name() string { return "inline_task_augment" }

// Preprocess walks history and re-injects approval context. Errors
// don't deny: a failed augmentation degrades context fidelity but
// doesn't fail the request.
//
// Requires non-empty UserID + AgentID on the request; without them
// the outcome store lookup can't scope correctly. Empty IDs yield
// Skip rather than a panic.
func (p *InlineTaskAugment) Preprocess(ctx context.Context, req pipeline.ReadOnlyRequest, mut pipeline.RequestMutator) (pipeline.RequestVerdict, error) {
	userID := req.UserID()
	agentID := req.AgentID()
	if userID == "" || agentID == "" {
		return pipeline.RequestVerdict{Outcome: pipeline.OutcomeSkip}, nil
	}
	if p.augment == nil {
		return pipeline.RequestVerdict{Outcome: pipeline.OutcomeSkip}, nil
	}

	augmented, err := p.augment(ctx, InlineTaskHistoryAugmentRequest{
		Body:     req.RawBody(),
		Provider: req.Provider(),
		UserID:   userID,
		AgentID:  agentID,
	})
	if err != nil {
		return pipeline.RequestVerdict{
			Outcome: pipeline.OutcomeSkip,
			Reason:  err.Error(),
			AuditParams: map[string]any{
				"inline_task_augment_error": err.Error(),
			},
		}, nil
	}
	if !augmented.Modified {
		return pipeline.RequestVerdict{Outcome: pipeline.OutcomeAllow}, nil
	}
	if err := mut.ReplaceBody(augmented.Body); err != nil {
		return pipeline.RequestVerdict{}, err
	}
	return pipeline.RequestVerdict{
		Outcome: pipeline.OutcomeAllow,
		AuditParams: map[string]any{
			"inline_task_history_augmented": true,
		},
	}, nil
}

var _ pipeline.RequestPolicy = (*InlineTaskAugment)(nil)
