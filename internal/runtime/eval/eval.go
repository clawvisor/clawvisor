// Package eval defines the typed evaluation surface — Outcomes,
// Facts, DecisionKind — that policies emit and audit consumers
// read. It is a leaf package: depends only on stdlib so any layer
// in the proxy can import it without inducing layering inversions.
//
// InspectorFact.Source is a plain string; inspector-specific
// translation lives at the policy / inspector boundary.
package eval

import "encoding/json"

// Outcome is the coarse verdict category an evaluator returns.
type Outcome string

const (
	OutcomeAllow        Outcome = "allow"
	OutcomeDeny         Outcome = "deny"
	OutcomeHold         Outcome = "hold"
	OutcomeRewrite      Outcome = "rewrite"
	OutcomeShortCircuit Outcome = "short_circuit"
	OutcomeSkip         Outcome = "skip"
)

// HeldKindHint classifies a verdict for postproc's coalescing pass.
type HeldKindHint string

const (
	HeldKindHintApproval HeldKindHint = "approval"
	HeldKindHintAllow    HeldKindHint = "allow"
	HeldKindHintRewrite  HeldKindHint = "rewrite"
	HeldKindHintDeny     HeldKindHint = "deny"
)

// BoundaryDenyReason categorizes a boundary check failure.
type BoundaryDenyReason string

const (
	BoundaryDenyReasonPlaceholderUnknown BoundaryDenyReason = "placeholder_unknown"
	BoundaryDenyReasonOwnershipMismatch  BoundaryDenyReason = "ownership_mismatch"
	BoundaryDenyReasonHostNotAllowed     BoundaryDenyReason = "host_not_allowed"
)

// DecisionKind is the coarse audit-row classification, matching the
// legacy three-value enum the audit store uses.
type DecisionKind string

const (
	DecisionAllow   DecisionKind = "allow"
	DecisionBlock   DecisionKind = "block"
	DecisionRewrite DecisionKind = "rewrite"
)

// EvaluationFact is the typed observation an evaluator emits about a
// tool_use. Facts are sum-types; consumers branch via type switch.
type EvaluationFact interface {
	isEvaluationFact()
}

// InspectorFact captures the inspector's classification of a tool_use.
// Source is a plain string (the inspector.VerdictSource string value)
// so this package stays a leaf — translation happens at the policy /
// inspector boundary.
type InspectorFact struct {
	Source       string
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

// ScriptSessionFact captures the script-session evaluator's outcome.
//
// Outcome is the audit-row label (e.g. "script_session_passthrough",
// "script_session_judge_allow", "script_session_judge_block").
//
// The Judge* fields are populated only on judge-consulted outcomes
// (script_session_judge_*). They give operators forensic visibility
// into a security-sensitive LLM decision: the prompt SHA pins which
// prompt revision produced the verdict, latency surfaces the
// hot-path cost, and the token counts let cost dashboards roll up
// judge invocation spend. JudgeError is populated when the judge
// returned an error and the evaluator fell through to the next chain
// stage — the audit row still records that an attempt happened.
//
// Zero-valued Judge* fields on non-judge outcomes are the expected
// shape; audit consumers should branch on Outcome before reading them.
type ScriptSessionFact struct {
	Outcome           string
	JudgePromptSHA    string
	JudgeLatencyMS    int64
	JudgeInputTokens  int
	JudgeOutputTokens int
	JudgeError        string
}

func (ScriptSessionFact) isEvaluationFact() {}

// AuthorizationFact captures the trigger-miss AuthorizationPolicy's
// outcome (the decision-engine Source string).
type AuthorizationFact struct {
	Outcome string
	Detail  string
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

// CredentialLocation describes where a credential placeholder appears
// in a tool_use input. Audit consumers store this on the audit row;
// providers / inspectors translate their domain types to this shape
// at the policy boundary.
type CredentialLocation struct {
	Kind   string
	Name   string
	Scheme string
}

// InspectorVerdictSnapshot is the audit-row projection of the
// inspector's verdict. Mirrors inspector.Verdict's emittable fields
// without importing the inspector package — kept here so AuditEvent
// stays in the conversation package without inducing a layering
// inversion.
type InspectorVerdictSnapshot struct {
	Source              string
	Host                string
	Method              string
	Path                string
	Reason              string
	IsAPICall           bool
	Ambiguous           bool
	Placeholders        []string
	CredentialLocations []CredentialLocation
}

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
		return DecisionBlock
	}
}

// MatchedTaskIDFromFacts walks a fact slice looking for the first
// TaskScopeFact carrying a MatchedTaskID.
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
// Outcome column expects.
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
		case IntentVerifyFact:
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
