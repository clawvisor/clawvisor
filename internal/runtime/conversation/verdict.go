package conversation

import (
	"github.com/clawvisor/clawvisor/internal/runtime/eval"
)

// Phase B leak cleanup: evaluation types now live in
// internal/runtime/eval. This file re-exports them as type aliases so
// the public conversation.X surface keeps working while breaking the
// conversation → inspector import (the InspectorFact.Source field is
// a plain string in eval; inspector.VerdictSource is converted at the
// policy/inspector boundary).

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

// ContinueSignal is returned by an evaluator when the tool_use is being
// served locally and the pipeline should re-enter with a synthetic
// continuation turn.
type ContinueSignal = eval.ContinueSignal

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
