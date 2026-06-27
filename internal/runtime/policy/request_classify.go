package policy

import (
	"context"

	"github.com/clawvisor/clawvisor/pkg/store"
)

const (
	ClassificationBelongsToExistingTask = "belongs_to_existing_task"
	ClassificationNeedsNewTask          = "needs_new_task"
	ClassificationOneOff                = "one_off"
	ClassificationAmbiguous             = "ambiguous"
)

type GatewayRequestClassification struct {
	Kind           string
	MatchedTask    *store.Task
	CandidateTasks []*store.Task
}

type GatewayRequestResolutionRequest struct {
	Classification GatewayRequestClassification
	ServiceType    string
	ServiceAlias   string
	Action         string
	Reason         string
	Params         map[string]any
}

type GatewayRequestResolver interface {
	Resolve(ctx context.Context, req GatewayRequestResolutionRequest) (GatewayRequestClassification, error)
}

func ClassifyGatewayRequest(tasks []*store.Task, agentID, serviceType, alias, action string) GatewayRequestClassification {
	return ClassifyGatewayRequestPreferred(tasks, agentID, serviceType, alias, action, "")
}

// ClassifyGatewayRequestPreferred picks the active task whose
// authorized_actions covers (serviceType/alias, action). When
// preferredTaskID is non-empty, classification is strictly scoped to
// that task: if the preferred task doesn't cover the action, the
// result is ClassificationNeedsNewTask (with the agent's other active
// tasks reported as candidates so the menu UI can still offer them
// for explicit re-checkout / expand) — NOT a silent match against a
// sibling task. This enforces per-conversation isolation when the
// caller has resolved a checked-out task.
func ClassifyGatewayRequestPreferred(tasks []*store.Task, agentID, serviceType, alias, action, preferredTaskID string) GatewayRequestClassification {
	candidates := make([]*store.Task, 0, len(tasks))
	for _, task := range tasks {
		if task == nil || task.AgentID != agentID || task.Status != "active" {
			continue
		}
		candidates = append(candidates, task)
	}

	if preferredTaskID != "" {
		for _, task := range candidates {
			if task.ID != preferredTaskID {
				continue
			}
			if matchTaskScope(task, serviceType, alias, action) {
				return GatewayRequestClassification{
					Kind:        ClassificationBelongsToExistingTask,
					MatchedTask: task,
				}
			}
			// Preferred task is active for this agent but doesn't
			// cover the action. Surface the full candidate set so the
			// caller can render a "switch task" menu, but DO NOT silently
			// match a different task.
			if len(candidates) > 0 {
				return GatewayRequestClassification{
					Kind:           ClassificationNeedsNewTask,
					CandidateTasks: candidates,
				}
			}
			return GatewayRequestClassification{Kind: ClassificationOneOff}
		}
		// Preferred task id was supplied but no active task with that id
		// exists for this agent (stale checkout, expired task, etc.).
		// Fall through to the normal candidate-pool path: this isn't a
		// scope leak — the preferred id is simply not a valid pointer.
	}

	inScope := make([]*store.Task, 0, len(candidates))
	for _, task := range candidates {
		if matchTaskScope(task, serviceType, alias, action) {
			inScope = append(inScope, task)
		}
	}

	switch len(inScope) {
	case 0:
		if len(candidates) > 0 {
			return GatewayRequestClassification{
				Kind:           ClassificationNeedsNewTask,
				CandidateTasks: candidates,
			}
		}
		return GatewayRequestClassification{Kind: ClassificationOneOff}
	case 1:
		return GatewayRequestClassification{
			Kind:        ClassificationBelongsToExistingTask,
			MatchedTask: inScope[0],
		}
	default:
		return GatewayRequestClassification{
			Kind:           ClassificationAmbiguous,
			CandidateTasks: inScope,
		}
	}
}

func matchTaskScope(task *store.Task, serviceType, alias, action string) bool {
	fullService := serviceType
	if alias != "" && alias != "default" {
		fullService = serviceType + ":" + alias
	}
	for _, authorized := range task.AuthorizedActions {
		if authorized.Service == fullService && (authorized.Action == action || authorized.Action == "*") {
			return true
		}
	}
	if fullService == serviceType {
		return false
	}
	for _, authorized := range task.AuthorizedActions {
		if authorized.Service == serviceType && (authorized.Action == action || authorized.Action == "*") {
			return true
		}
	}
	return false
}
