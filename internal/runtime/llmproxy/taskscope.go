package llmproxy

import (
	"context"
	"fmt"

	"github.com/clawvisor/clawvisor/internal/runtime/policy"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// TaskScopeDecision is the result of a per-tool-use task-scope check.
type TaskScopeDecision struct {
	// Allowed is true when the agent has an active task whose
	// AuthorizedActions covers (ServiceID, ActionID).
	Allowed bool

	// TaskID is the matched task's ID when Allowed; empty otherwise.
	TaskID string

	// Reason is a short human-readable explanation. For Allowed=true,
	// it names the matched task. For Allowed=false, it names what was
	// missing (no active task vs. no scope match).
	Reason string

	// Ambiguous is true when more than one active task covers the
	// request. v0 treats this as Allowed (the gateway has the same
	// behavior), but the audit row records it for visibility.
	Ambiguous bool
}

// TaskScopeChecker authorizes a tool_use call against the calling agent's
// active task scopes. The lite-proxy postprocess layer queries this after
// the inspector classifies a tool_use as an API call and BoundaryCheck
// confirms the host. A denied scope check is a hard refusal — the response
// is rewritten to a Clawvisor refusal rather than passing through.
type TaskScopeChecker interface {
	Check(ctx context.Context, userID, agentID, serviceID, actionID string) TaskScopeDecision
}

// StoreTaskScopeChecker reads tasks from the store and runs the same
// classification logic the gateway uses.
type StoreTaskScopeChecker struct {
	store store.Store
}

// NewStoreTaskScopeChecker builds a checker that reads from the live store.
// The userID is the agent's owner; it scopes the ListTasks query.
func NewStoreTaskScopeChecker(s store.Store) *StoreTaskScopeChecker {
	return &StoreTaskScopeChecker{store: s}
}

// Check runs the per-tool-use task-scope authorization. Behavior:
//   - empty service/action: Allowed=false, Reason="unresolved_action".
//   - no active tasks for the agent: Allowed=false, Reason="no_active_task".
//   - active task(s) cover the action: Allowed=true with TaskID.
//   - active task(s) exist but none cover the action: Allowed=false,
//     Reason="needs_new_task" — caller can route to approval flow.
func (c *StoreTaskScopeChecker) Check(ctx context.Context, userID, agentID, serviceID, actionID string) TaskScopeDecision {
	if c == nil || c.store == nil {
		return TaskScopeDecision{Reason: "no_task_store_configured"}
	}
	if userID == "" || agentID == "" {
		return TaskScopeDecision{Reason: "no_agent_context"}
	}
	if serviceID == "" || actionID == "" {
		return TaskScopeDecision{Reason: "unresolved_action"}
	}
	tasks, _, err := c.store.ListTasks(ctx, userID, store.TaskFilter{ActiveOnly: true})
	if err != nil {
		return TaskScopeDecision{Reason: fmt.Sprintf("list_tasks_error: %s", err.Error())}
	}
	classification := policy.ClassifyGatewayRequest(tasks, agentID, serviceID, "", actionID)
	return classifyToDecision(classification)
}

func classifyToDecision(c policy.GatewayRequestClassification) TaskScopeDecision {
	switch c.Kind {
	case policy.ClassificationBelongsToExistingTask:
		if c.MatchedTask != nil {
			return TaskScopeDecision{Allowed: true, TaskID: c.MatchedTask.ID, Reason: "matched task " + c.MatchedTask.ID}
		}
		return TaskScopeDecision{Allowed: true, Reason: "matched task (id missing)"}
	case policy.ClassificationAmbiguous:
		var picked *store.Task
		if len(c.CandidateTasks) > 0 {
			picked = c.CandidateTasks[0]
		}
		d := TaskScopeDecision{Allowed: true, Ambiguous: true, Reason: "ambiguous: multiple active tasks cover this action"}
		if picked != nil {
			d.TaskID = picked.ID
		}
		return d
	case policy.ClassificationNeedsNewTask:
		return TaskScopeDecision{Reason: "needs_new_task"}
	case policy.ClassificationOneOff:
		return TaskScopeDecision{Reason: "no_active_task"}
	}
	return TaskScopeDecision{Reason: "unknown_classification"}
}
