package policies

import (
	"context"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/pipeline"
)

// ScriptSessionEvaluator passes through tool_uses that are already
// shaped for the proxy's resolver mount via a script-session caller
// token. These calls carry a cv-script-* token in X-Clawvisor-Caller
// and a URL targeting the resolver — running the inspector chain on
// them would try to "rewrite" an already-rewritten curl and fail.
//
// Runs after ControlToolUseEvaluator and before InspectorChain. The
// gate requires BOTH the script-session header AND a URL pointing at
// the resolver host; mismatched off-proxy curls fall through to the
// inspector chain.
type ScriptSessionEvaluator struct {
	resolver ScriptSessionResolver
}

// ScriptSessionInputs is the per-call bundle supplied by the host.
// ResolverBaseURL is the proxy's /api/proxy mount; an empty value
// disables the policy (the inspector chain handles all tool_uses).
type ScriptSessionInputs struct {
	ResolverBaseURL string
}

// ScriptSessionResolver returns per-call inputs. Returning nil makes
// the evaluator Skip.
type ScriptSessionResolver func(ctx context.Context, tu conversation.ToolUse) *ScriptSessionInputs

// NewScriptSessionEvaluator constructs the evaluator. A nil resolver
// makes it always Skip.
func NewScriptSessionEvaluator(resolver ScriptSessionResolver) *ScriptSessionEvaluator {
	return &ScriptSessionEvaluator{resolver: resolver}
}

// Name returns the audit-friendly identifier.
func (ScriptSessionEvaluator) Name() string { return "script_session" }

// Evaluate returns OutcomeAllow when the tool_use is a recognized
// script-session call; otherwise Skip.
func (e *ScriptSessionEvaluator) Evaluate(ctx context.Context, _ pipeline.ReadOnlyResponse, tu conversation.ToolUse, _ pipeline.ToolUseMutator) (pipeline.ToolUseVerdict, error) {
	if e.resolver == nil {
		return pipeline.ToolUseVerdict{Outcome: pipeline.OutcomeSkip}, nil
	}
	in := e.resolver(ctx, tu)
	if in == nil || in.ResolverBaseURL == "" {
		return pipeline.ToolUseVerdict{Outcome: pipeline.OutcomeSkip}, nil
	}
	if !llmproxy.ScriptSessionToolUse(tu.Input, in.ResolverBaseURL) {
		return pipeline.ToolUseVerdict{Outcome: pipeline.OutcomeSkip}, nil
	}
	return pipeline.ToolUseVerdict{
		Outcome: pipeline.OutcomeAllow,
		Reason:  "tool_use carries a script-session caller token; resolver enforces scope",
		AuditFields: map[string]any{
			"path":           "script_session_passthrough",
			"verdict_source": "script_session",
		},
		Facts: []pipeline.EvaluationFact{pipeline.ScriptSessionFact{Outcome: "script_session_passthrough"}},
	}, nil
}

var _ pipeline.ToolUseEvaluator = (*ScriptSessionEvaluator)(nil)
