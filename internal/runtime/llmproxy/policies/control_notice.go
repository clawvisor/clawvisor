package policies

import (
	"context"
	"strings"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/controltool"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/pipeline"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// ControlNotice injects the Clawvisor control-plane notice into the
// request's system prompt, advertising the control API surface
// (/api/control/tasks, vault placeholders, etc.) and any active tool
// rules.
//
// Gating:
//   - Empty ControlBaseURL → Skip.
//   - URL path ending in `/count_tokens` → Skip.
//   - Request declares no tools[] → Skip (no point advertising the
//     control API to a model with no tool affordance).
//   - Notice already present in the body → Skip (idempotent).
//
// Dependencies:
//   - ControlBaseURL is fixed at construction (handler config).
//   - ToolRules / AvailableTools are recomputed per request via the
//     callbacks provided at construction. The handler owns the loaders
//     so the policy stays decoupled from the Store.
type ControlNotice struct {
	controlBaseURL   string
	availableTools   AvailableToolsFn
	loadToolRules    ToolRulesLoader
	loadActiveTasks  ActiveTasksSnapshotLoader
}

// AvailableToolsFn extracts the declared tool names from a request.
// The policy receives this shape via a loader closure so it does not
// depend on the handler's request-debug helpers.
type AvailableToolsFn func(provider conversation.Provider, body []byte) []string

// ToolRulesLoader loads the active tool-rule policy for the given
// user/agent. Returns nil on best-effort error so notice injection
// remains non-fatal.
type ToolRulesLoader func(ctx context.Context, userID, agentID string) []*store.RuntimePolicyRule

// ActiveTasksSnapshotLoader renders the conversation-start snapshot of
// active tasks for the calling agent. The string is embedded verbatim
// in the ACTIVE TASKS section of the control notice; an empty string is
// fine and renders the empty-state copy. Returns "" on best-effort
// error so notice injection remains non-fatal — the agent just falls
// back to GET /control/tasks if it cares about live state.
type ActiveTasksSnapshotLoader func(ctx context.Context, userID, agentID string) string

// NewControlNotice constructs the policy. controlBaseURL "" skips.
// availableTools and loadToolRules nil → Skip on every request. The
// snapshot loader is optional — pass nil to inject the notice without
// the ACTIVE TASKS section (legacy callers).
func NewControlNotice(controlBaseURL string, availableTools AvailableToolsFn, loadToolRules ToolRulesLoader) *ControlNotice {
	return NewControlNoticeWithSnapshot(controlBaseURL, availableTools, loadToolRules, nil)
}

// NewControlNoticeWithSnapshot is the snapshot-aware constructor. The
// snapshot loader runs once on first-turn injection (the existing
// sentinel-based dedup in InjectControlNoticeWithSnapshot keeps the
// snapshot frozen for cache stability on later turns).
func NewControlNoticeWithSnapshot(controlBaseURL string, availableTools AvailableToolsFn, loadToolRules ToolRulesLoader, loadActiveTasks ActiveTasksSnapshotLoader) *ControlNotice {
	return &ControlNotice{
		controlBaseURL:  controlBaseURL,
		availableTools:  availableTools,
		loadToolRules:   loadToolRules,
		loadActiveTasks: loadActiveTasks,
	}
}

// Name returns the audit-friendly policy identifier.
func (ControlNotice) Name() string { return "control_notice" }

// Preprocess injects the notice when gates pass.
func (p *ControlNotice) Preprocess(ctx context.Context, req pipeline.ReadOnlyRequest, mut pipeline.RequestMutator) (pipeline.RequestVerdict, error) {
	if p.controlBaseURL == "" || p.availableTools == nil {
		return pipeline.RequestVerdict{Outcome: pipeline.OutcomeSkip}, nil
	}
	if h := req.HTTPRequest(); h != nil && strings.HasSuffix(h.URL.Path, "/count_tokens") {
		return pipeline.RequestVerdict{Outcome: pipeline.OutcomeSkip}, nil
	}

	tools := p.availableTools(req.Provider(), req.RawBody())
	if len(tools) == 0 {
		return pipeline.RequestVerdict{Outcome: pipeline.OutcomeSkip}, nil
	}

	var rules []*store.RuntimePolicyRule
	if p.loadToolRules != nil {
		rules = p.loadToolRules(ctx, req.UserID(), req.AgentID())
	}

	var activeTasks string
	if p.loadActiveTasks != nil {
		activeTasks = p.loadActiveTasks(ctx, req.UserID(), req.AgentID())
	}

	injected, modified, err := controltool.InjectControlNoticeWithSnapshot(req.Provider(), req.RawBody(), p.controlBaseURL, tools, rules, activeTasks)
	if err != nil {
		return pipeline.RequestVerdict{
			Outcome: pipeline.OutcomeDeny,
			Reason:  ModelSafeInternalReason("control notice injection"),
			AuditParams: map[string]any{
				"deny_outcome":         "malformed_request",
				"control_notice_error": err.Error(),
			},
		}, nil
	}
	if !modified {
		return pipeline.RequestVerdict{Outcome: pipeline.OutcomeSkip}, nil
	}
	if err := mut.ReplaceBody(injected); err != nil {
		return pipeline.RequestVerdict{}, err
	}
	return pipeline.RequestVerdict{
		Outcome: pipeline.OutcomeAllow,
		AuditParams: map[string]any{
			"control_notice_injected": true,
		},
	}, nil
}

var _ pipeline.RequestPolicy = (*ControlNotice)(nil)
