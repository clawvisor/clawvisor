package conversation

import (
	"github.com/clawvisor/clawvisor/internal/runtime/eval"
)

// Evaluation types live in internal/runtime/eval. This file re-exports
// them as type aliases so the public conversation.X surface keeps
// working while conversation stays independent of the inspector package.

// Outcome is the coarse verdict category. See eval.Outcome.
type Outcome = eval.Outcome

const (
	OutcomeAllow        = eval.OutcomeAllow
	OutcomeDeny         = eval.OutcomeDeny
	OutcomeHold         = eval.OutcomeHold
	OutcomeRewrite      = eval.OutcomeRewrite
	OutcomeShortCircuit = eval.OutcomeShortCircuit
	OutcomeSkip         = eval.OutcomeSkip
)

// HeldKindHint classifies a verdict for postproc's coalescing pass.
type HeldKindHint = eval.HeldKindHint

const (
	HeldKindHintApproval = eval.HeldKindHintApproval
	HeldKindHintAllow    = eval.HeldKindHintAllow
	HeldKindHintRewrite  = eval.HeldKindHintRewrite
	HeldKindHintDeny     = eval.HeldKindHintDeny
)

// BoundaryDenyReason categorizes a boundary check failure.
type BoundaryDenyReason = eval.BoundaryDenyReason

const (
	BoundaryDenyReasonPlaceholderUnknown = eval.BoundaryDenyReasonPlaceholderUnknown
	BoundaryDenyReasonOwnershipMismatch  = eval.BoundaryDenyReasonOwnershipMismatch
	BoundaryDenyReasonHostNotAllowed     = eval.BoundaryDenyReasonHostNotAllowed
)

// EvaluationFact is the typed observation an evaluator emits about a
// tool_use.
type EvaluationFact = eval.EvaluationFact

// Fact type aliases. Source on InspectorFact is a plain string (from
// inspector.VerdictSource at the policy boundary).
type (
	InspectorFact     = eval.InspectorFact
	TaskScopeFact     = eval.TaskScopeFact
	RewriteFact       = eval.RewriteFact
	ControlFact       = eval.ControlFact
	IntentVerifyFact  = eval.IntentVerifyFact
	BoundaryFact      = eval.BoundaryFact
	ScriptSessionFact = eval.ScriptSessionFact
	AuthorizationFact = eval.AuthorizationFact
)

// RecoverableDenyVerdict builds the standard recoverable-deny verdict
// used by evaluators that block on a construction error the agent can
// fix on its next attempt (inspector parse error, boundary check
// failure, credential rewrite error, malformed control body, etc.).
//
// The postproc eval wrapper reads RecoverableReason and transforms the
// verdict into the canonical placeholder + pending-substitution shape:
// the model's original tool_use is preserved (restored on the next
// inbound /v1/messages) and the reason text lands as the tool_result
// content. SubstituteWith stays populated as a terminal fallback for
// non-placeholder transport paths (e.g. registry unavailable).
func RecoverableDenyVerdict(reason string, facts ...EvaluationFact) ToolUseVerdict {
	return ToolUseVerdict{
		Outcome:           OutcomeDeny,
		Reason:            reason,
		SubstituteWith:    reason,
		RecoverableReason: reason,
		Facts:             facts,
	}
}

// InspectorVerdictSnapshot is the audit-row projection of the
// inspector's verdict.
type InspectorVerdictSnapshot = eval.InspectorVerdictSnapshot

// CredentialLocation describes where a credential placeholder appears
// in a tool_use input.
type CredentialLocation = eval.CredentialLocation

// AuditEvent is the typed per-tool-use audit record. Carries:
//   - the pipeline-domain observation (Outcome, Decision, Reason, Facts)
//   - the audit wire-shape needed by the emitter (InspectorVerdict,
//     TaskID, EvaluatorName, Winning, OutcomeName)
//
// InspectorVerdict is an eval.InspectorVerdictSnapshot (mirror of
// inspector.Verdict's emittable fields). Inspector translation happens
// at the policy/inspector boundary; this keeps conversation from
// importing the inspector package.
type AuditEvent struct {
	ToolUse          ToolUse
	EvaluatorName    string
	Outcome          Outcome
	OutcomeName      string
	Decision         DecisionKind
	Reason           string
	Facts            []EvaluationFact
	Winning          bool
	InspectorVerdict eval.InspectorVerdictSnapshot
	TaskID           string

	// AnnotationFacts carries facts from non-winning evaluators for the
	// same tool_use, surfaced so the audit row can record forensic
	// signals (judge invocation cost, scope detail) that the chain's
	// upstream stages produced before yielding to the winner. Kept
	// separate from Facts so OutcomeName/MatchedTaskID lookups still
	// derive from the winning evaluator's verdict, not from a Skip's
	// side-channel record.
	AnnotationFacts []EvaluationFact
}

// DecisionKind is the coarse audit-row classification.
type DecisionKind = eval.DecisionKind

const (
	DecisionAllow   = eval.DecisionAllow
	DecisionBlock   = eval.DecisionBlock
	DecisionRewrite = eval.DecisionRewrite
)

// DecisionFromOutcome maps an Outcome to the coarse Decision the audit
// store expects. Hold and Deny both collapse to "block".
func DecisionFromOutcome(o Outcome) DecisionKind { return eval.DecisionFromOutcome(o) }

// MatchedTaskIDFromFacts walks a fact slice looking for the first
// TaskScopeFact carrying a MatchedTaskID.
func MatchedTaskIDFromFacts(facts []EvaluationFact) string {
	return eval.MatchedTaskIDFromFacts(facts)
}

// OutcomeNameFromFacts extracts the stage-specific outcome name from
// a verdict's typed Facts.
func OutcomeNameFromFacts(evaluatorName string, outcome Outcome, facts []EvaluationFact) string {
	return eval.OutcomeNameFromFacts(evaluatorName, outcome, facts)
}
