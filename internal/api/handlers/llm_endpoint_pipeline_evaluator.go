package handlers

import (
	"context"
	"net/http"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/pipeline"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/policies"
)

// pipelineToolUseEvaluatorFactory is the handler's implementation of
// llmproxy.ToolUseEvaluatorFactory — builds the six-stage policies
// chain and runs it via pipeline.BridgeToolUseEvaluator. Audit rows
// flow through the supplied emit callback (verified byte-identical to
// legacy emission by TestLegacyAndPipelineEmitters_ProduceIdenticalAuditRows).
//
// Migration scope: control + script-session + credential-rewrite +
// boundary-check stages run fully through the policies chain.
// Trigger-miss authorization and credentialed-path TaskScope delegate
// back to llmproxy.BuildLegacyToolUseEvaluator (built per-call)
// because their hold management + approvalPrompt rendering hasn't
// migrated yet. Each delegated stage emits its audit row via the
// legacy closure's audit() → emit path; non-delegated stages emit via
// the policies emitter. The two paths are routed through the same
// emit callback so any single decision audit-rows once.
var pipelineToolUseEvaluatorFactory llmproxy.ToolUseEvaluatorFactory = func(
	req *http.Request,
	cfg llmproxy.PostprocessConfig,
	provider conversation.Provider,
	emit func(llmproxy.BufferedAudit),
) conversation.ToolUseEvaluator {
	legacy := llmproxy.BuildLegacyToolUseEvaluator(req, cfg, provider, emit)

	triggerMissAuth := func(_ context.Context, tu conversation.ToolUse, mut pipeline.ToolUseMutator) pipeline.ToolUseVerdict {
		convV := legacy(tu)
		if mut != nil {
			if len(convV.RewriteInput) > 0 {
				_ = mut.RewriteArgs(convV.RewriteInput)
			}
			if convV.SubstituteWith != "" {
				_ = mut.ReplaceWithText(convV.SubstituteWith)
			}
		}
		return delegatePipelineVerdict(convV)
	}

	credentialedTaskScope := func(_ context.Context, _ conversation.ToolUse) llmproxy.TaskScopeDecision {
		// Skip — credentialed-path authorization delegates via the
		// trigger-miss authorizer's fall-through above. Empty Reason
		// makes the policy emit Skip.
		return llmproxy.TaskScopeDecision{}
	}

	chain := policies.ComposeToolUseEvaluatorChain(policies.ToolUseChainConfig{
		Control:         buildPipelineControlResolver(cfg),
		ScriptSession:   buildPipelineScriptSessionResolver(cfg),
		Inspector:       cfg.Inspector,
		AllowedHostsFor: nil, // boundary-check fall-through for now
		TriggerMissAuth: triggerMissAuth,
		TaskScope:       credentialedTaskScope,
		Rewrite:         buildPipelineRewriteResolver(cfg),
	})

	return func(tu conversation.ToolUse) conversation.ToolUseVerdict {
		ctx := req.Context()
		res := &singletonToolUseResponse{provider: provider, tu: tu}
		evalFn, result, err := pipeline.BridgeToolUseEvaluator(ctx, res, []conversation.ToolUse{tu}, chain)
		if err != nil {
			return legacy(tu)
		}
		// If the trigger-miss authorizer fired, the legacy closure
		// already emitted its audit row through emit. Skip the
		// policies emitter to avoid double-emit. Otherwise the
		// pipeline stages need their emit pass.
		if !triggerMissAuthorizerFired(result, tu.ID) {
			policies.EmitToolUseAuditRows(ctx, result, []conversation.ToolUse{tu}, cfg.Inspector, func(_ context.Context, row policies.ToolUseAuditRow) {
				emit(llmproxy.BufferedAudit{
					ToolUse:  row.ToolUse,
					Verdict:  row.Verdict,
					Decision: row.Decision,
					Outcome:  row.Outcome,
					Reason:   row.Reason,
					TaskID:   row.TaskID,
				})
			})
		}
		return evalFn(tu)
	}
}

func delegatePipelineVerdict(v conversation.ToolUseVerdict) pipeline.ToolUseVerdict {
	outcome := pipeline.OutcomeDeny
	if v.Allowed {
		if len(v.RewriteInput) > 0 {
			outcome = pipeline.OutcomeRewrite
		} else {
			outcome = pipeline.OutcomeAllow
		}
	}
	return pipeline.ToolUseVerdict{
		Outcome: outcome,
		Reason:  v.Reason,
	}
}

// triggerMissAuthorizerFired detects whether the InspectorChain's
// terminal verdict for tuID came from the delegated trigger-miss
// authorizer. Suppresses double-emit when the legacy closure has
// already pushed its audit row.
func triggerMissAuthorizerFired(result *pipeline.ToolUseResult, tuID string) bool {
	if result == nil {
		return false
	}
	for _, ev := range result.Evaluations {
		if ev.ToolUseID != tuID || ev.Verdict.Outcome == pipeline.OutcomeSkip {
			continue
		}
		if ev.EvaluatorName != "inspector_chain" {
			return false
		}
		if ev.Verdict.AuditFields == nil {
			return true
		}
		if _, present := ev.Verdict.AuditFields["boundary_check_passed"]; present {
			return false
		}
		if _, present := ev.Verdict.AuditFields["inspector_source"]; present {
			return false
		}
		return true
	}
	return false
}

type singletonToolUseResponse struct {
	provider conversation.Provider
	tu       conversation.ToolUse
}

func (r *singletonToolUseResponse) Provider() conversation.Provider       { return r.provider }
func (r *singletonToolUseResponse) StreamShape() conversation.StreamShape {
	return conversation.StreamShapeUnknown
}
func (r *singletonToolUseResponse) IsStreaming() bool                     { return false }
func (r *singletonToolUseResponse) ToolUses() []conversation.ToolUse {
	return []conversation.ToolUse{r.tu}
}

func buildPipelineControlResolver(cfg llmproxy.PostprocessConfig) policies.ControlToolUseResolver {
	if cfg.ControlBaseURL == "" {
		return nil
	}
	controlBaseURL := cfg.ControlBaseURL
	agentID := cfg.AgentID
	cache := cfg.CallerNonces
	return func(_ context.Context, _ conversation.ToolUse) *policies.ControlToolUseInputs {
		return &policies.ControlToolUseInputs{
			ControlBaseURL:  controlBaseURL,
			AgentID:         agentID,
			CallerNonces:    cache,
			InterceptInline: nil,
		}
	}
}

func buildPipelineScriptSessionResolver(cfg llmproxy.PostprocessConfig) policies.ScriptSessionResolver {
	if cfg.RewriteOpts.ResolverBaseURL == "" {
		return nil
	}
	resolverBaseURL := cfg.RewriteOpts.ResolverBaseURL
	return func(_ context.Context, _ conversation.ToolUse) *policies.ScriptSessionInputs {
		return &policies.ScriptSessionInputs{ResolverBaseURL: resolverBaseURL}
	}
}

func buildPipelineRewriteResolver(cfg llmproxy.PostprocessConfig) policies.CredentialRewriteResolver {
	if cfg.Inspector == nil {
		return nil
	}
	insp := cfg.Inspector
	opts := cfg.RewriteOpts
	cache := cfg.CallerNonces
	agentID := cfg.AgentID
	return func(_ context.Context, _ conversation.ToolUse) *policies.CredentialRewriteInputs {
		return &policies.CredentialRewriteInputs{
			Inspector:    insp,
			CallerNonces: cache,
			AgentID:      agentID,
			RewriteOpts:  opts,
		}
	}
}
