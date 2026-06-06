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
// and runs each tool_use through it via pipeline.RunToolUseEvaluators.
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
	toolUses []conversation.ToolUse,
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
		// EvaluateTriggerMissAuthorization emits its own audit row via the
		// emit callback; flag the verdict so the downstream emission pass
		// suppresses a second row for this tool_use.
		pv := conversationToPipelineVerdict(convV)
		pv.EmittedAuditExternally = true
		return pv
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

	chain := policies.ComposeToolUseEvaluatorChain(policies.ToolUseChainConfig{
		Control:         buildControlResolver(req, cfg, provider, emit),
		ScriptSession:   buildScriptSessionResolver(cfg),
		Inspector:       cfg.Inspector,
		Boundary:        buildBoundaryResolver(cfg),
		TriggerMissAuth: triggerMissAuth,
		ReadOnlyShell:   buildReadOnlyShellResolver(cfg),
		TaskScope:       credentialedTaskScope,
		Rewrite:         buildRewriteResolver(cfg),
	})

	// Response-level orchestration: callers (buffered + streaming
	// postproc) supply the full tool_use list so the pipeline runs
	// ONCE on the sibling set. The returned eval is a verdict lookup;
	// audit rows + holds emit up-front during this single pass.
	//
	// Empty toolUses returns a no-op eval (no tools, nothing to do).
	if len(toolUses) == 0 {
		return func(_ conversation.ToolUse) conversation.ToolUseVerdict {
			return conversation.ToolUseVerdict{Allowed: true}
		}
	}
	ctx := req.Context()
	res := &multiToolUseResponse{provider: provider, toolUses: toolUses}
	evalFn, result, err := pipeline.RunToolUseEvaluators(ctx, res, toolUses, chain)
	if err != nil {
		// Pipeline errored before producing per-tool verdicts. Emit
		// one audit row keyed to the first tool (best signal we have)
		// and return a Deny-for-everything evaluator so the rewriter
		// renders refusals consistently.
		firstTU := toolUses[0]
		emit(llmproxy.BufferedAudit{
			ToolUse:     firstTU,
			Decision:    conversation.DecisionBlock,
			OutcomeName: "pipeline_error",
			Reason:      err.Error(),
		})
		errMsg := "Clawvisor: authorization pipeline failed — " + err.Error()
		return func(_ conversation.ToolUse) conversation.ToolUseVerdict {
			return conversation.ToolUseVerdict{Allowed: false, Reason: errMsg}
		}
	}
	// Emit one audit row per tool_use whose verdict didn't externally
	// emit. Skipped tools (trigger-miss authorizer fired its own
	// emit) are correctly suppressed.
	matchedTaskIDs := make(map[string]string, len(toolUses))
	for _, tu := range toolUses {
		if verdictEmittedAuditExternally(result, tu.ID) {
			continue
		}
		matchedTaskIDs[tu.ID] = lookupMatchedTaskID(ctx, cfg, tu)
	}
	toEmit := make([]conversation.ToolUse, 0, len(toolUses))
	for _, tu := range toolUses {
		if !verdictEmittedAuditExternally(result, tu.ID) {
			toEmit = append(toEmit, tu)
		}
	}
	policies.EmitToolUseAuditRows(ctx, result, toEmit, cfg.Inspector, func(_ context.Context, row policies.ToolUseAuditRow) {
		if row.TaskID == "" {
			if id := matchedTaskIDs[row.ToolUse.ID]; id != "" {
				row.TaskID = id
			}
		}
		emit(llmproxy.BufferedAudit{
			ToolUse:          row.ToolUse,
			InspectorVerdict: row.Verdict,
			Decision:         conversation.DecisionKind(row.Decision),
			OutcomeName:      row.Outcome,
			Reason:           row.Reason,
			TaskID:           row.TaskID,
		})
	})
	return evalFn
}

// multiToolUseResponse is the pipeline ReadOnlyResponse the
// response-level factory hands to RunToolUseEvaluators. Carries
// the full sibling set so the orchestrator's per-tool loop sees all
// of them in one pass.
type multiToolUseResponse struct {
	provider conversation.Provider
	toolUses []conversation.ToolUse
}

func (r *multiToolUseResponse) Provider() conversation.Provider { return r.provider }
func (r *multiToolUseResponse) StreamShape() conversation.StreamShape {
	return conversation.StreamShapeUnknown
}
func (r *multiToolUseResponse) IsStreaming() bool             { return false }
func (r *multiToolUseResponse) ToolUses() []conversation.ToolUse { return r.toolUses }

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

// verdictEmittedAuditExternally reports whether the winning verdict
// for tu already emitted its audit row via the legacy emit callback
// (trigger-miss authorizer pattern). When true, the downstream
// EmitToolUseAuditRows pass skips this tool_use to avoid a duplicate row.
func verdictEmittedAuditExternally(result *pipeline.ToolUseResult, tuID string) bool {
	if result == nil {
		return false
	}
	for _, ev := range result.Evaluations {
		if ev.ToolUseID != tuID || ev.Verdict.Outcome == pipeline.OutcomeSkip {
			continue
		}
		return ev.Verdict.EmittedAuditExternally
	}
	return false
}

// Legacy singletonToolUseResponse + its methods have been removed —
// every postproc caller now supplies the full sibling set to the
// factory and the orchestrator runs response-level. multiToolUseResponse
// is the canonical pipeline-side response type.

func buildControlResolver(req *http.Request, cfg llmproxy.PostprocessConfig, provider conversation.Provider, emit func(llmproxy.BufferedAudit)) policies.ControlToolUseResolver {
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
						ToolUse:     tu,
						Decision:    conversation.DecisionKind(decision),
						OutcomeName: outcome,
						Reason:      reason,
					})
				}
				traceFn := func(_ string, _ ...any) {}
				convV, claimed := llmproxy.MaybeInterceptInlineTaskDefinition(req, cfg, auditFn, traceFn, provider, tu, call)
				if !claimed {
					return pipeline.ToolUseVerdict{}, false
				}
				return conversationToPipelineVerdict(convV), true
			},
		}
	}
}

// conversationToPipelineVerdict sets the typed Outcome on a verdict
// returned from a legacy helper that only set Allowed. After Phase 8
// the verdict type is unified, so the only field-level translation
// needed is deriving Outcome from Allowed.
func conversationToPipelineVerdict(v conversation.ToolUseVerdict) pipeline.ToolUseVerdict {
	if v.Outcome != "" {
		return v
	}
	if v.Allowed {
		if len(v.RewriteInput) > 0 {
			v.Outcome = pipeline.OutcomeRewrite
		} else {
			v.Outcome = pipeline.OutcomeAllow
		}
	} else {
		v.Outcome = pipeline.OutcomeDeny
	}
	return v
}

// buildBoundaryResolver wires InspectorChain's boundary check to the
// placeholder store, running the three discrete checks the legacy
// boundaryCheckVerdict combined into one binary:
//   1. placeholder exists in the store
//   2. placeholder is owned by the calling agent
//   3. target host is in the placeholder's bound-service allowlist
// Each failure mode returns a distinct BoundaryDenyReason so audit
// rows tell operators WHICH check rejected the call instead of always
// reading "host not allowed."
//
// Without this defense-in-depth, an autovault placeholder belonging
// to a different agent could be sent to any host that looked like it
// accepted that credential; the resolver would still catch it at the
// network boundary, but the proxy's pre-flight check would have
// silently passed.
func buildBoundaryResolver(cfg llmproxy.PostprocessConfig) policies.BoundaryResolver {
	if cfg.Store == nil {
		return nil
	}
	st := cfg.Store
	userID := cfg.AgentUserID
	agentID := cfg.AgentID
	return func(ctx context.Context, v inspector.Verdict) policies.BoundaryDecision {
		var placeholder string
		if len(v.Placeholders) > 0 {
			placeholder = v.Placeholders[0]
		}
		rec, err := st.GetRuntimePlaceholder(ctx, placeholder)
		if err != nil || rec == nil {
			return policies.BoundaryDecision{
				DenyReason: pipeline.BoundaryDenyReasonPlaceholderUnknown,
				Reason:     "Clawvisor: autovault placeholder not found in store",
			}
		}
		if _, ok := llmproxy.ValidateRuntimePlaceholderAccess(ctx, st, rec, userID, agentID, time.Now().UTC()); !ok {
			return policies.BoundaryDecision{
				DenyReason: pipeline.BoundaryDenyReasonOwnershipMismatch,
				Reason:     "Clawvisor: autovault placeholder belongs to a different agent",
			}
		}
		hosts, _ := llmproxy.RuntimePlaceholderBoundHosts(ctx, st, rec)
		ok, reason := inspector.BoundaryCheck(v, hosts)
		decision := policies.BoundaryDecision{Allowed: ok, AllowedHosts: hosts, Reason: reason}
		if !ok {
			decision.DenyReason = pipeline.BoundaryDenyReasonHostNotAllowed
		}
		return decision
	}
}

// buildReadOnlyShellResolver wires ReadOnlyShellPassthroughPolicy +
// SensitivePathPolicy to PostprocessConfig's AgentID + ToolRules.
func buildReadOnlyShellResolver(cfg llmproxy.PostprocessConfig) policies.ReadOnlyShellResolver {
	agentID := cfg.AgentID
	toolRules := cfg.ToolRules
	if agentID == "" && toolRules == nil {
		return nil
	}
	return func(_ context.Context, _ conversation.ToolUse) *policies.ReadOnlyShellInputs {
		return &policies.ReadOnlyShellInputs{
			AgentID:   agentID,
			ToolRules: toolRules,
		}
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
