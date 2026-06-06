package policies

import (
	"context"
	"strings"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/pipeline"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// ControlNotice injects the Clawvisor control-plane notice into the
// request's system prompt, advertising the control API surface
// (/api/control/tasks, vault placeholders, etc.) and any active tool
// rules.
//
// Gating mirrors today's handler:
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
	controlBaseURL string
	availableTools AvailableToolsFn
	loadToolRules  ToolRulesLoader
}

// AvailableToolsFn extracts the declared tool names from a request.
// Today's handler computes this via liteProxyRequestDebugSummary;
// the policy receives the same shape via the loader closure.
type AvailableToolsFn func(provider conversation.Provider, body []byte) []string

// ToolRulesLoader loads the active tool-rule policy for the given
// user/agent. Returns nil on best-effort error — today's handler
// treats notice injection as best-effort.
type ToolRulesLoader func(ctx context.Context, userID, agentID string) []*store.RuntimePolicyRule

// NewControlNotice constructs the policy. controlBaseURL "" skips.
// availableTools and loadToolRules nil → Skip on every request.
func NewControlNotice(controlBaseURL string, availableTools AvailableToolsFn, loadToolRules ToolRulesLoader) *ControlNotice {
	return &ControlNotice{
		controlBaseURL: controlBaseURL,
		availableTools: availableTools,
		loadToolRules:  loadToolRules,
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

	injected, modified, err := llmproxy.InjectControlNoticeWithPolicy(req.Provider(), req.RawBody(), p.controlBaseURL, tools, rules)
	if err != nil {
		return pipeline.RequestVerdict{
			Outcome: pipeline.OutcomeDeny,
			Reason:  err.Error(),
			AuditParams: map[string]any{
				"deny_outcome": "malformed_request",
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
