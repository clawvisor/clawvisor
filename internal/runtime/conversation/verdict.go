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

// ContinueSignal is returned by an evaluator when the tool_use is being
// served locally and the pipeline should re-enter with a synthetic
// continuation turn.
type ContinueSignal struct {
	SyntheticAssistantBlocks []json.RawMessage
	SyntheticToolResults     []json.RawMessage
	PrependNotice            string
}
