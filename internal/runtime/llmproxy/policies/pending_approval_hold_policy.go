package policies

import (
	"context"
	"fmt"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/inspector"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/pipeline"
)

// PendingApprovalHoldPolicy converts an OutcomeHold verdict (from an
// upstream AuthorizationPolicy) into a held call: it submits the hold
// to the PendingApprovals cache, renders the approval prompt with the
// resulting approval ID, and replaces the verdict's SubstituteWith
// with the rendered prompt.
//
// The orchestrator's first-non-Skip-wins rule means this policy only
// runs when no upstream policy claimed the verdict. Hold verdicts from
// AuthorizationPolicy are handled through its AuthorizationHoldHandler.
type PendingApprovalHoldPolicy struct {
	resolver PendingApprovalHoldResolver
}

// PendingApprovalHoldResolver returns the per-call inputs the policy
// needs. Returning nil makes the policy Skip (no approval cache or
// inputs configured).
type PendingApprovalHoldResolver func(ctx context.Context, tu conversation.ToolUse, v inspector.Verdict) *PendingApprovalHoldInputs

// PendingApprovalHoldInputs is the per-call bundle the host supplies.
type PendingApprovalHoldInputs struct {
	Holder         PendingApprovalHolder
	UserID         string
	AgentID        string
	Provider       conversation.Provider
	ConversationID string
}

// PendingApprovalHolder is the narrow interface this policy needs from
// the host-side PendingApprovalCache (avoiding a direct llmproxy
// import). The host wraps PendingApprovalCache.Hold via a thin shim.
type PendingApprovalHolder interface {
	Hold(ctx context.Context, req HoldRequest) (HoldResult, error)
}

// HoldRequest is the typed input passed to PendingApprovalHolder.Hold.
type HoldRequest struct {
	ToolUse          conversation.ToolUse
	InspectorVerdict inspector.Verdict
	Reason           string
}

// HoldResult is the typed output of PendingApprovalHolder.Hold.
type HoldResult struct {
	ApprovalID string
}

// NewPendingApprovalHoldPolicy constructs the policy. nil resolver →
// Skip-always.
func NewPendingApprovalHoldPolicy(resolver PendingApprovalHoldResolver) *PendingApprovalHoldPolicy {
	return &PendingApprovalHoldPolicy{resolver: resolver}
}

// Name returns the audit-friendly evaluator identifier.
func (PendingApprovalHoldPolicy) Name() string { return "pending_approval_hold" }

// Evaluate rejects standalone chain usage. The actual hold side-effect
// is invoked by AuthorizationPolicy via its AuthorizationHoldHandler;
// wiring this docs-anchor type directly would otherwise silently
// pass-through approval-required calls.
func (p *PendingApprovalHoldPolicy) Evaluate(context.Context, pipeline.ReadOnlyResponse, conversation.ToolUse, pipeline.ToolUseMutator) (pipeline.ToolUseVerdict, error) {
	return pipeline.ToolUseVerdict{}, fmt.Errorf("pending_approval_hold is not valid as a standalone ToolUseEvaluator; use AuthorizationPolicy hold handling")
}

var _ pipeline.ToolUseEvaluator = (*PendingApprovalHoldPolicy)(nil)
