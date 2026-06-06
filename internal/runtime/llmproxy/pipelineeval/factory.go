// Package pipelineeval exposes the policies-chain-based
// llmproxy.ToolUseEvaluatorFactory as Factory. It's a leaf package
// over llmproxy + llmproxy/policies + llmproxy/pipeline so handlers
// and llmproxy's own internal tests can both import it without
// re-introducing the policies → llmproxy cycle the broader refactor
// already navigated.
//
// The factory composes the six-stage chain (ControlToolUseEvaluator
// + ScriptSessionEvaluator + InspectorChain with TriggerMissAuthorizer
// + TaskScopeEvaluator + IntentVerifyEvaluator + CredentialRewriteEvaluator)
// and runs each tool_use through it via pipeline.BridgeToolUseEvaluator.
// Trigger-miss and credentialed-path authorization run through the
// exported llmproxy.Evaluate* helpers; the inline task-definition
// intercept flows through ControlToolUseEvaluator's InterceptInline
// hook. Audit emission flows through policies.EmitToolUseAuditRows
// into the BufferedAudit emit callback the caller supplies.
//
// Verified byte-equivalent to legacy newToolUseEvaluator emission by
// TestLegacyAndPipelineEmitters_ProduceIdenticalAuditRows.
package pipelineeval

import (
	"context"
	"net/http"
	"time"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/inspector"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/pipeline"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/policies"
	runtimedecision "github.com/clawvisor/clawvisor/pkg/runtime/decision"
)

// Factory is the llmproxy.ToolUseEvaluatorFactory implementation that
// drives the policies-chain-based tool_use evaluation. Assign it to
// PostprocessConfig.ToolUseEvaluatorFactory to opt a call into the
// new path.
var Factory llmproxy.ToolUseEvaluatorFactory = func(
	req *http.Request,
	cfg llmproxy.PostprocessConfig,
	provider conversation.Provider,
	emit func(llmproxy.BufferedAudit),
) conversation.ToolUseEvaluator {
	triggerMissAuth := func(ctx context.Context, tu conversation.ToolUse, mut pipeline.ToolUseMutator) pipeline.ToolUseVerdict {
		v := cfg.Inspector.Inspect(ctx, inspector.ToolUse{
			ID:    tu.ID,
			Name:  tu.Name,
			Input: tu.Input,
		})
		if v.Source != inspector.SourceTriggerMiss && inspector.AllPlaceholdersAreStubs(v.Placeholders) {
			v = inspector.Verdict{
				IsAPICall: false,
				Source:    inspector.SourceTriggerMiss,
				Reason:    "placeholders are stub-length (no real vault reference)",
			}
		}
		convV := llmproxy.EvaluateTriggerMissAuthorization(ctx, cfg, provider, tu, v, emit)
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

	credentialedTaskScope := func(ctx context.Context, tu conversation.ToolUse) llmproxy.TaskScopeDecision {
		v := cfg.Inspector.Inspect(ctx, inspector.ToolUse{
			ID:    tu.ID,
			Name:  tu.Name,
			Input: tu.Input,
		})
		if !v.IsAPICall || v.Ambiguous {
			return llmproxy.TaskScopeDecision{}
		}
		result := llmproxy.EvaluateCredentialedAuthorization(ctx, cfg, provider, tu, v, emit)
		if result.Allowed {
			return llmproxy.TaskScopeDecision{}
		}
		return llmproxy.TaskScopeDecision{
			Allowed: false,
			Reason:  result.Verdict.Reason,
			TaskID:  result.MatchedTaskID,
		}
	}

	inlineExtras := make(map[string]conversation.ToolUseVerdict)
	chain := policies.ComposeToolUseEvaluatorChain(policies.ToolUseChainConfig{
		Control:         buildControlResolver(req, cfg, provider, emit, inlineExtras),
		ScriptSession:   buildScriptSessionResolver(cfg),
		Inspector:       cfg.Inspector,
		AllowedHostsFor: buildAllowedHostsResolver(cfg),
		TriggerMissAuth: triggerMissAuth,
		TaskScope:       credentialedTaskScope,
		Rewrite:         buildRewriteResolver(cfg),
	})

	return func(tu conversation.ToolUse) conversation.ToolUseVerdict {
		ctx := req.Context()
		res := &singletonToolUseResponse{provider: provider, tu: tu}
		evalFn, result, err := pipeline.BridgeToolUseEvaluator(ctx, res, []conversation.ToolUse{tu}, chain)
		if err != nil {
			emit(llmproxy.BufferedAudit{
				ToolUse:  tu,
				Decision: "block",
				Outcome:  "pipeline_error",
				Reason:   err.Error(),
			})
			return conversation.ToolUseVerdict{
				Allowed: false,
				Reason:  "Clawvisor: authorization pipeline failed — " + err.Error(),
			}
		}
		if !triggerMissAuthorizerFired(result, tu.ID) {
			matchedTaskID := lookupMatchedTaskID(ctx, cfg, tu)
			policies.EmitToolUseAuditRows(ctx, result, []conversation.ToolUse{tu}, cfg.Inspector, func(_ context.Context, row policies.ToolUseAuditRow) {
				if row.TaskID == "" && matchedTaskID != "" {
					row.TaskID = matchedTaskID
				}
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
		return applyInlineExtras(inlineExtras, tu, evalFn(tu))
	}
}

func lookupMatchedTaskID(ctx context.Context, cfg llmproxy.PostprocessConfig, tu conversation.ToolUse) string {
	if cfg.Inspector == nil {
		return ""
	}
	v := cfg.Inspector.Inspect(ctx, inspector.ToolUse{
		ID:    tu.ID,
		Name:  tu.Name,
		Input: tu.Input,
	})
	if !v.IsAPICall || v.Ambiguous || v.Host == "" {
		return ""
	}
	var serviceID, actionID string
	if cfg.Catalog != nil {
		if resolved, ok := cfg.Catalog.Resolve(v.Host, v.Method, v.Path); ok {
			serviceID = resolved.ServiceID
			actionID = resolved.ActionID
		}
	}
	if cfg.CandidateTasks == nil && cfg.ToolRules == nil && cfg.EgressRules == nil {
		if cfg.TaskScope == nil || serviceID == "" || actionID == "" {
			return ""
		}
		dec := cfg.TaskScope.Check(ctx, cfg.AgentUserID, cfg.AgentID, serviceID, actionID)
		return dec.TaskID
	}
	decisionInput := runtimedecision.AuthorizationInput{
		ToolUse:         tu,
		UserID:          cfg.AgentUserID,
		AgentID:         cfg.AgentID,
		Posture:         cfg.Posture,
		Target:          runtimedecision.TargetRequest{Host: v.Host, Method: v.Method, Path: v.Path},
		Service:         serviceID,
		Action:          actionID,
		CandidateTasks:  cfg.CandidateTasks,
		ToolRules:       cfg.ToolRules,
		EgressRules:     cfg.EgressRules,
		PreferredTaskID: cfg.PreferredTaskID,
	}
	dec, err := runtimedecision.EvaluateAuthorization(ctx, decisionInput)
	if err != nil {
		return ""
	}
	if dec.Task != nil {
		return dec.Task.ID
	}
	return ""
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

func (r *singletonToolUseResponse) Provider() conversation.Provider { return r.provider }
func (r *singletonToolUseResponse) StreamShape() conversation.StreamShape {
	return conversation.StreamShapeUnknown
}
func (r *singletonToolUseResponse) IsStreaming() bool { return false }
func (r *singletonToolUseResponse) ToolUses() []conversation.ToolUse {
	return []conversation.ToolUse{r.tu}
}

func buildControlResolver(req *http.Request, cfg llmproxy.PostprocessConfig, provider conversation.Provider, emit func(llmproxy.BufferedAudit), inlineExtras map[string]conversation.ToolUseVerdict) policies.ControlToolUseResolver {
	if cfg.ControlBaseURL == "" {
		return nil
	}
	controlBaseURL := cfg.ControlBaseURL
	agentID := cfg.AgentID
	cache := cfg.CallerNonces
	return func(_ context.Context, _ conversation.ToolUse) *policies.ControlToolUseInputs {
		return &policies.ControlToolUseInputs{
			ControlBaseURL: controlBaseURL,
			AgentID:        agentID,
			CallerNonces:   cache,
			InterceptInline: func(_ context.Context, tu conversation.ToolUse, call llmproxy.ControlCall) (pipeline.ToolUseVerdict, bool) {
				auditFn := func(decision, outcome, reason string) {
					emit(llmproxy.BufferedAudit{
						ToolUse:  tu,
						Decision: decision,
						Outcome:  outcome,
						Reason:   reason,
					})
				}
				traceFn := func(_ string, _ ...any) {}
				convV, claimed := llmproxy.MaybeInterceptInlineTaskDefinition(req, cfg, auditFn, traceFn, provider, tu, call)
				if !claimed {
					return pipeline.ToolUseVerdict{}, false
				}
				// Stash the conversation-shape extras so the
				// per-tool factory closure can surface them.
				inlineExtras[tu.ID] = convV
				return delegatePipelineVerdict(convV), true
			},
		}
	}
}

// applyInlineExtras patches the conversation verdict the bridge
// returned with the inline-intercept extras the resolver captured.
// Used by the per-tool closure right before returning the verdict
// to the rewriter so SubstituteWith / RewriteInput from
// MaybeInterceptInlineTaskDefinition survive the pipeline.ToolUseVerdict
// round trip.
func applyInlineExtras(extras map[string]conversation.ToolUseVerdict, tu conversation.ToolUse, v conversation.ToolUseVerdict) conversation.ToolUseVerdict {
	x, ok := extras[tu.ID]
	if !ok {
		return v
	}
	if x.SubstituteWith != "" {
		v.SubstituteWith = x.SubstituteWith
	}
	if len(x.RewriteInput) > 0 {
		v.RewriteInput = x.RewriteInput
	}
	if x.ContinueWithToolResult != "" {
		v.ContinueWithToolResult = x.ContinueWithToolResult
	}
	if x.PrependAssistantNotice != "" {
		v.PrependAssistantNotice = x.PrependAssistantNotice
	}
	if x.CreatedTaskID != "" {
		v.CreatedTaskID = x.CreatedTaskID
	}
	return v
}

// buildAllowedHostsResolver wires InspectorChain's boundary check to
// the placeholder store. The legacy boundaryCheckVerdict combined
// three checks: (1) placeholder exists, (2) ownership matches the
// agent, (3) target host in the placeholder's bound-service allowlist.
// We compose the same three here: returning an empty allowlist (which
// makes the chain's BoundaryCheck fail) for any of (1)/(2)/(3) failing,
// returning the bound hosts otherwise.
//
// Without this, an autovault placeholder belonging to a different agent
// could be sent to any host that "looked like" it accepted that
// credential; the resolver would still catch it on the actual call, but
// the proxy's defense-in-depth boundary check would be bypassed.
func buildAllowedHostsResolver(cfg llmproxy.PostprocessConfig) policies.AllowedHostsResolver {
	if cfg.Store == nil {
		return nil
	}
	st := cfg.Store
	userID := cfg.AgentUserID
	agentID := cfg.AgentID
	return func(ctx context.Context, placeholder string) []string {
		rec, err := st.GetRuntimePlaceholder(ctx, placeholder)
		if err != nil || rec == nil {
			return nil
		}
		if _, ok := llmproxy.ValidateRuntimePlaceholderAccess(ctx, st, rec, userID, agentID, time.Now().UTC()); !ok {
			return nil
		}
		hosts, _ := llmproxy.RuntimePlaceholderBoundHosts(ctx, st, rec)
		return hosts
	}
}

func buildScriptSessionResolver(cfg llmproxy.PostprocessConfig) policies.ScriptSessionResolver {
	if cfg.RewriteOpts.ResolverBaseURL == "" {
		return nil
	}
	resolverBaseURL := cfg.RewriteOpts.ResolverBaseURL
	return func(_ context.Context, _ conversation.ToolUse) *policies.ScriptSessionInputs {
		return &policies.ScriptSessionInputs{ResolverBaseURL: resolverBaseURL}
	}
}

func buildRewriteResolver(cfg llmproxy.PostprocessConfig) policies.CredentialRewriteResolver {
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
