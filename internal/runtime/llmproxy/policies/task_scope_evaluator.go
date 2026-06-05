package policies

import (
	"context"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/pipeline"
)

// TaskScopeEvaluator authorizes a tool_use against the agent's active
// task scopes. Runs after the inspector chain classifies the call.
//
// The decision lookup is supplied by the handler as a closure so the
// evaluator stays decoupled from identity carriers and store
// internals. The handler bakes (userID, agentID, store, catalog) into
// the closure at construction time; the evaluator just dispatches.
//
// Outcomes:
//   - Decision.Reason == "" or no checker → Skip
//   - Decision.Allowed → Allow with matched_task_id in AuditFields
//   - Decision.Allowed == false → Hold with HoldKey
//     "needs_task_<toolu_id>". The orchestrator's coalescing rules
//     decide whether to merge with siblings.
type TaskScopeEvaluator struct {
	resolver TaskScopeResolver
}

// TaskScopeResolver returns the task-scope decision for a tool_use.
// The handler implements this by closing over (userID, agentID,
// TaskScopeChecker, catalog) — identity carriers stay out of the
// evaluator signature.
//
// Returns Decision with empty Reason when the tool_use shouldn't be
// scope-checked (e.g., inspector didn't classify it as an API call).
type TaskScopeResolver func(ctx context.Context, tu conversation.ToolUse) llmproxy.TaskScopeDecision

// NewTaskScopeEvaluator constructs the evaluator. nil resolver → Skip
// on every tool_use.
func NewTaskScopeEvaluator(resolver TaskScopeResolver) *TaskScopeEvaluator {
	return &TaskScopeEvaluator{resolver: resolver}
}

// CredentialedTarget is the inspector-classified target a credentialed
// tool_use intends to reach. The handler-side resolver uses this to
// populate runtimedecision.AuthorizationInput.Target when wrapping
// EvaluateAuthorization for the credentialed path. Exposed as a
// stable type so the resolver contract is documented in the policy
// package (not just convention inside the handler).
type CredentialedTarget struct {
	Host    string
	Method  string
	Path    string
	Service string // ServiceID resolved via catalog (empty if unresolved)
	Action  string // ActionID resolved via catalog (empty if unresolved)
}

// CredentialedTaskScopeResolver is the variant of TaskScopeResolver
// the handler builds when a tool_use has been classified as
// credentialed (Inspector verdict.IsAPICall == true). It receives the
// resolved target alongside the tool_use so the underlying
// runtimedecision.EvaluateAuthorization call can populate Target,
// Service, and Action correctly. The handler converts a
// CredentialedTaskScopeResolver to TaskScopeResolver via a small
// adapter closure that supplies the target from the inspector verdict.
type CredentialedTaskScopeResolver func(ctx context.Context, tu conversation.ToolUse, target CredentialedTarget) llmproxy.TaskScopeDecision

// Name returns the audit-friendly identifier.
func (TaskScopeEvaluator) Name() string { return "task_scope" }

// Evaluate dispatches to the resolver and translates the decision
// into a pipeline verdict.
func (e *TaskScopeEvaluator) Evaluate(ctx context.Context, _ pipeline.ReadOnlyResponse, tu conversation.ToolUse, _ pipeline.ToolUseMutator) (pipeline.ToolUseVerdict, error) {
	if e.resolver == nil {
		return pipeline.ToolUseVerdict{Outcome: pipeline.OutcomeSkip}, nil
	}
	dec := e.resolver(ctx, tu)
	if dec.Reason == "" {
		// Resolver chose not to act on this tool_use (e.g., not an
		// API call). Let downstream evaluators / default-Allow handle it.
		return pipeline.ToolUseVerdict{Outcome: pipeline.OutcomeSkip}, nil
	}

	fields := map[string]any{
		"task_scope_reason":    dec.Reason,
		"task_scope_allowed":   dec.Allowed,
		"task_scope_ambiguous": dec.Ambiguous,
	}
	if dec.TaskID != "" {
		fields["matched_task_id"] = dec.TaskID
	}

	if dec.Allowed {
		return pipeline.ToolUseVerdict{
			Outcome:     pipeline.OutcomeAllow,
			AuditFields: fields,
		}, nil
	}

	// Scope check failed — hold for approval. Per-tool HoldKey so
	// disparate scope failures don't all collapse into one prompt
	// unless the legacy code says they should (see ShouldCoalesce).
	return pipeline.ToolUseVerdict{
		Outcome:     pipeline.OutcomeHold,
		Reason:      dec.Reason,
		AuditFields: fields,
		HoldKey:     "needs_task_" + tu.ID,
	}, nil
}

var _ pipeline.ToolUseEvaluator = (*TaskScopeEvaluator)(nil)
