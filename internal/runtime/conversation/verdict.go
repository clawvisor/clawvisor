package conversation

import (
	"encoding/json"

	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/inspector"
)

// Outcome is the coarse verdict category an evaluator returns. The
// pipeline orchestrator's first-non-Skip-wins rule operates on this.
// Audit rows already use these labels; the names match for symmetry.
type Outcome string

const (
	// OutcomeAllow — request/response continues unchanged.
	OutcomeAllow Outcome = "allow"
	// OutcomeDeny — terminate the chain at this policy. Subsequent
	// policies do not run. Audit records the deny.
	OutcomeDeny Outcome = "deny"
	// OutcomeHold — the tool_use needs human approval before proceeding.
	OutcomeHold Outcome = "hold"
	// OutcomeRewrite — the tool_use args have been rewritten in-place.
	OutcomeRewrite Outcome = "rewrite"
	// OutcomeShortCircuit — skip remaining pre policies AND the upstream
	// forward step; return a synthetic response to the client.
	OutcomeShortCircuit Outcome = "short_circuit"
	// OutcomeSkip — this policy declined to act. No audit, no mutations.
	OutcomeSkip Outcome = "skip"
)

// HeldKindHint classifies a verdict for postproc's coalescing pass.
// Evaluators that want to influence coalescing set it explicitly;
// empty means classifyVerdict falls back to substring matching on
// Reason. Phase 6 deletes the fallback.
type HeldKindHint string

const (
	HeldKindHintApproval HeldKindHint = "approval"
	HeldKindHintAllow    HeldKindHint = "allow"
	HeldKindHintRewrite  HeldKindHint = "rewrite"
	HeldKindHintDeny     HeldKindHint = "deny"
)

// EvaluationFact is the typed observation an evaluator emits about a
// tool_use. Facts are typed sum-types; consumers (audit emitter,
// coalescing, telemetry) branch via type switch instead of reading
// magic string keys from a map[string]any.
//
// Facts are recorded for EVERY evaluator that runs against a tool_use,
// including those returning Skip without claiming the verdict —
// observation is a separate channel from verdict claiming.
type EvaluationFact interface {
	isEvaluationFact()
}

// InspectorFact captures the inspector's classification of a tool_use:
// trigger-miss vs API call, host/method/path, placeholders, ambiguity.
type InspectorFact struct {
	Source       inspector.VerdictSource
	Host         string
	Method       string
	Path         string
	Placeholders []string
	IsAPICall    bool
	Ambiguous    bool
	Reason       string
}

func (InspectorFact) isEvaluationFact() {}

// TaskScopeFact captures the task-scope check outcome.
type TaskScopeFact struct {
	Reason        string
	Allowed       bool
	MatchedTaskID string
	Ambiguous     bool
}

func (TaskScopeFact) isEvaluationFact() {}

// RewriteFact captures the credential-rewrite outcome.
type RewriteFact struct {
	Outcome      string
	TargetHost   string
	TargetMethod string
	TargetPath   string
}

func (RewriteFact) isEvaluationFact() {}

// ControlFact captures the control-tool-use evaluator's outcome.
type ControlFact struct {
	Outcome       string
	Path          string
	Method        string
	SyntheticHost string
}

func (ControlFact) isEvaluationFact() {}

// IntentVerifyFact captures the LLM intent-verifier outcome.
type IntentVerifyFact struct {
	Mode        string
	Allowed     bool
	Explanation string
	Outcome     string
}

func (IntentVerifyFact) isEvaluationFact() {}

// BoundaryFact captures the boundary-check outcome for credentialed
// tool_uses.
type BoundaryFact struct {
	Passed      bool
	DenyReason  BoundaryDenyReason
	Reason      string
	Placeholder string
	Host        string
}

func (BoundaryFact) isEvaluationFact() {}

// BoundaryDenyReason categorizes a boundary check failure.
type BoundaryDenyReason string

const (
	BoundaryDenyReasonPlaceholderUnknown BoundaryDenyReason = "placeholder_unknown"
	BoundaryDenyReasonOwnershipMismatch  BoundaryDenyReason = "ownership_mismatch"
	BoundaryDenyReasonHostNotAllowed     BoundaryDenyReason = "host_not_allowed"
)

// ScriptSessionFact captures the script-session evaluator's outcome.
type ScriptSessionFact struct {
	Outcome string
}

func (ScriptSessionFact) isEvaluationFact() {}

// AuthorizationFact captures the trigger-miss AuthorizationPolicy's
// outcome (the decision-engine Source string: "rule_allow",
// "task_scope", "task_scope_missing", "decision_error", etc.).
// Preferred over TaskScopeFact's generic "matched_task_scope" naming
// when the decision Source is known.
type AuthorizationFact struct {
	Outcome string
}

func (AuthorizationFact) isEvaluationFact() {}

// ContinueSignal is returned by an evaluator when the tool_use is being
// served locally and the pipeline should re-enter with a synthetic
// continuation turn.
type ContinueSignal struct {
	SyntheticAssistantBlocks []json.RawMessage
	SyntheticToolResults     []json.RawMessage
	PrependNotice            string
}

// AuditEvent is the typed per-tool-use audit record. Carries:
//   - the pipeline-domain observation (Outcome, Decision, Reason, Facts)
//   - the audit wire-shape needed by the emitter (InspectorVerdict,
//     TaskID, EvaluatorName, Winning, OutcomeName)
//
// Replaces the legacy BufferedAudit + LogToolUseInspected positional
// arg list. Translation to store.AuditEntry happens in the emitter.
type AuditEvent struct {
	// ToolUse is the assistant tool_use block the verdict applies to.
	ToolUse ToolUse
	// EvaluatorName names the policy that produced the verdict.
	EvaluatorName string
	// Outcome is the typed verdict category (allow/deny/hold/rewrite/skip).
	Outcome Outcome
	// OutcomeName is the stage-specific outcome string that lands in the
	// audit store's Outcome column (e.g., "task_scope_missing",
	// "caller_nonce_unavailable", "pass_through"). Derived from typed
	// Facts by outcomeNameFor, or set directly by legacy emitters.
	OutcomeName string
	// Decision is the coarse audit-row classification.
	Decision DecisionKind
	// Reason is the human-readable explanation.
	Reason string
	// Facts is the typed observation set the evaluator emitted.
	Facts []EvaluationFact
	// Winning reports whether this event corresponds to the verdict
	// that won the tool_use's evaluation (first non-Skip in the chain).
	Winning bool
	// InspectorVerdict is the inspector's classification for this
	// tool_use, surfaced into the store row's params blob.
	InspectorVerdict inspector.Verdict
	// TaskID names the active task this tool_use matched.
	TaskID string
}

// DecisionKind is the coarse audit-row classification, matching the
// legacy three-value enum the audit store uses.
type DecisionKind string

const (
	DecisionAllow   DecisionKind = "allow"
	DecisionBlock   DecisionKind = "block"
	DecisionRewrite DecisionKind = "rewrite"
)

// DecisionFromOutcome maps an Outcome to the coarse Decision the audit
// store expects. Hold and Deny both collapse to "block".
func DecisionFromOutcome(o Outcome) DecisionKind {
	switch o {
	case OutcomeAllow:
		return DecisionAllow
	case OutcomeRewrite:
		return DecisionRewrite
	case OutcomeDeny, OutcomeHold:
		return DecisionBlock
	default:
		return DecisionAllow
	}
}

// MatchedTaskIDFromFacts walks a fact slice looking for the first
// TaskScopeFact carrying a MatchedTaskID. TaskScope evaluators may
// emit the fact on Skip paths (e.g., credentialed rewrite where
// TaskScope sees the match but CredentialRewrite claims the verdict),
// so the audit emitter aggregates facts across the trail.
func MatchedTaskIDFromFacts(facts []EvaluationFact) string {
	for _, f := range facts {
		if tf, ok := f.(TaskScopeFact); ok && tf.MatchedTaskID != "" {
			return tf.MatchedTaskID
		}
	}
	return ""
}

// OutcomeNameFromFacts extracts the stage-specific outcome name from
// a verdict's typed Facts. Each evaluator's Fact carries the outcome
// string directly; this helper produces the value the audit store's
// Outcome column expects. Falls back to a generic name per Outcome
// when no fact matches.
func OutcomeNameFromFacts(evaluatorName string, outcome Outcome, facts []EvaluationFact) string {
	for _, f := range facts {
		switch ff := f.(type) {
		case AuthorizationFact:
			if ff.Outcome != "" {
				return ff.Outcome
			}
		case ControlFact:
			if ff.Outcome != "" {
				return ff.Outcome
			}
		case RewriteFact:
			if ff.Outcome != "" {
				return ff.Outcome
			}
		case ScriptSessionFact:
			if ff.Outcome != "" {
				return ff.Outcome
			}
		case TaskScopeFact:
			if ff.Reason != "" {
				if ff.Allowed {
					return "matched_task_scope"
				}
				return "task_scope_missing"
			}
		case BoundaryFact:
			if !ff.Passed {
				return "boundary_check_failed"
			}
		}
	}
	switch outcome {
	case OutcomeAllow:
		switch evaluatorName {
		case "inspector_chain":
			return "boundary_check_passed"
		case "script_session":
			return "script_session_passthrough"
		default:
			return "pass_through"
		}
	case OutcomeRewrite:
		return "success"
	case OutcomeDeny:
		return "deny"
	case OutcomeHold:
		return "approval_required"
	default:
		return ""
	}
}
