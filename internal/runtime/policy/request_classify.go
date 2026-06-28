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
		// Strict mode: every code path here MUST either match the
		// preferred task or return NeedsNewTask. Falling through to
		// the candidate-pool match below would let a sibling task
		// silently authorize the call — the exact cross-conversation
		// leak this isolation fix exists to prevent. That includes the
		// stale-checkout case (preferred id supplied but no active task
		// with that id), which used to fall through under the
		// rationale "it's just a dangling pointer." Cubic flagged that
		// rationale as a leak window: a checked-out task that expired
		// mid-conversation should not implicitly switch the
		// authorization target.
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
			break
		}
		// Any non-empty preferredTaskID that doesn't resolve to a
		// covering active task → NeedsNewTask. CandidateTasks may be
		// empty when the agent's other active tasks have all expired,
		// but the kind must still be NeedsNewTask (not OneOff): the
		// conversation HAD a checkout, so this isn't a brand-new
		// agent. OneOff would be semantically wrong and would
		// indicate to the audit row that no checkout had ever been
		// recorded, defeating per-conversation isolation telemetry.
		return GatewayRequestClassification{
			Kind:           ClassificationNeedsNewTask,
			CandidateTasks: candidates,
		}
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
