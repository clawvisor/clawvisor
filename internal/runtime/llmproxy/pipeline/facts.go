package pipeline

import "github.com/clawvisor/clawvisor/internal/runtime/llmproxy/inspector"

// EvaluationFact is the typed observation an evaluator emits about a
// tool_use. Facts are typed sum-types; consumers (audit emitter,
// coalescing, telemetry) branch via type switch instead of reading
// magic string keys from a map[string]any.
//
// Facts are recorded for EVERY evaluator that runs against a tool_use,
// including those returning Skip without claiming the verdict —
// observation is a separate channel from verdict claiming. An
// InspectorChain that returns Skip on a credentialed boundary-pass
// (so downstream CredentialRewrite can claim) still emits its
// InspectorFact, which the audit row built from the winning
// CredentialRewrite verdict reads. Without this rule the audit emitter
// would never see TaskScope's matched_task_id when CredentialRewrite
// is the winner.
type EvaluationFact interface {
	isEvaluationFact()
}

// InspectorFact captures the inspector's classification of a tool_use:
// trigger-miss vs API call, host/method/path, placeholders, ambiguity.
// Emitted by InspectorChain (and the standalone InspectorEvaluator) on
// every tool_use it sees, regardless of whether it claims the verdict.
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

// TaskScopeFact captures the task-scope check outcome. Emitted by
// TaskScopeEvaluator and by the credentialed-task-scope branch of
// InspectorChain. The MatchedTaskID is the resolved task ID even when
// the verdict is Hold/Deny (so audit rows can link to the same task
// the user ultimately needs to approve).
type TaskScopeFact struct {
	Reason        string
	Allowed       bool
	MatchedTaskID string
	Ambiguous     bool
}

func (TaskScopeFact) isEvaluationFact() {}

// RewriteFact captures the credential-rewrite outcome. Emitted by
// CredentialRewriteEvaluator.
type RewriteFact struct {
	// Outcome is one of "success", "caller_nonce_unavailable",
	// "rewriter_error", "rewriter_inapplicable".
	Outcome      string
	TargetHost   string
	TargetMethod string
	TargetPath   string
}

func (RewriteFact) isEvaluationFact() {}

// ControlFact captures the control-tool-use evaluator's outcome.
// Emitted by ControlToolUseEvaluator.
type ControlFact struct {
	// Outcome is one of "rewrite_synthetic_host",
	// "rewrite_failure", "intercept_inline_task",
	// "intercept_inline_task_failure", "ambiguous_passthrough".
	Outcome       string
	Path          string
	Method        string
	SyntheticHost string
}

func (ControlFact) isEvaluationFact() {}

// IntentVerifyFact captures the LLM intent-verifier outcome.
// Emitted by IntentVerifyEvaluator.
type IntentVerifyFact struct {
	Mode        string // "off" | "lenient" | "strict"
	Allowed     bool
	Explanation string
	// Outcome is one of "" (allowed), "verifier_circuit_open",
	// "verifier_error".
	Outcome string
}

func (IntentVerifyFact) isEvaluationFact() {}

// BoundaryFact captures the boundary-check outcome for credentialed
// tool_uses. Emitted by BoundaryCheckEvaluator (and the boundary check
// embedded in InspectorChain). Reason names the failing check:
// "placeholder_unknown", "ownership_mismatch", "host_not_allowed".
type BoundaryFact struct {
	Passed      bool
	Reason      string
	Placeholder string
	Host        string
}

func (BoundaryFact) isEvaluationFact() {}

// ScriptSessionFact captures the script-session evaluator's outcome.
// Emitted by ScriptSessionEvaluator on resolver-mediated requests.
type ScriptSessionFact struct {
	Outcome string // "session_passthrough", "session_unbound", etc.
}

func (ScriptSessionFact) isEvaluationFact() {}
