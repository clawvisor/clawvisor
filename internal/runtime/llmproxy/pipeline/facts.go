package pipeline

import "github.com/clawvisor/clawvisor/internal/runtime/conversation"

// EvaluationFact aliases conversation.EvaluationFact. Phase 8 lives in
// conversation so response rewriters and pipeline evaluators share the
// same typed observation channel without a bridge translation.
type EvaluationFact = conversation.EvaluationFact

// Each concrete fact type is an alias for the conversation-side
// equivalent.
type (
	InspectorFact     = conversation.InspectorFact
	TaskScopeFact     = conversation.TaskScopeFact
	RewriteFact       = conversation.RewriteFact
	ControlFact       = conversation.ControlFact
	IntentVerifyFact  = conversation.IntentVerifyFact
	BoundaryFact      = conversation.BoundaryFact
	ScriptSessionFact = conversation.ScriptSessionFact
)

// BoundaryDenyReason aliases conversation.BoundaryDenyReason.
type BoundaryDenyReason = conversation.BoundaryDenyReason

const (
	BoundaryDenyReasonPlaceholderUnknown = conversation.BoundaryDenyReasonPlaceholderUnknown
	BoundaryDenyReasonOwnershipMismatch  = conversation.BoundaryDenyReasonOwnershipMismatch
	BoundaryDenyReasonHostNotAllowed     = conversation.BoundaryDenyReasonHostNotAllowed
)
