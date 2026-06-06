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
// into the conversation.AuditEvent emit callback the caller supplies.
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
	"github.com/clawvisor/clawvisor/pkg/runtime/toolnames"
	"github.com/clawvisor/clawvisor/pkg/store"
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
	emit func(conversation.AuditEvent),
) conversation.ToolUseEvaluator {
	credentialedTaskScope := buildCredentialedTaskScope(
		cfg.AgentContext,
		cfg.AuditContext,
		cfg.AuthorizationContext,
		cfg.ApprovalContext,
		cfg.RewriteContext,
		provider,
		emit,
	)

	chain := policies.ComposeToolUseEvaluatorChain(policies.ToolUseChainConfig{
		Control:       buildControlResolver(req, cfg.AgentContext, cfg.AuditContext, cfg.ApprovalContext, cfg.RewriteContext, cfg.RoutingContext, provider, emit),
		ScriptSession: buildScriptSessionResolver(cfg.RewriteContext),
		Inspector:     cfg.Inspector,
		Boundary:      buildBoundaryResolver(cfg.AgentContext, cfg.Store),
		ReadOnlyShell: buildReadOnlyShellResolver(cfg.AgentContext, cfg.AuthorizationContext),
		Authorization: buildAuthorizationResolver(cfg.AgentContext, cfg.AuditContext, cfg.AuthorizationContext, cfg.ApprovalContext, cfg.RewriteContext, provider),
		TaskScope:     credentialedTaskScope,
		Rewrite:       buildRewriteResolver(cfg.AgentContext, cfg.RewriteContext),
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
		emit(conversation.AuditEvent{
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
	// Emit one audit row per tool_use. The legacy trigger-miss
	// suppression channel (verdictEmittedAuditExternally) was deleted
	// with Phase 6 — no evaluator emits side-channel audits anymore.
	matchedTaskIDs := make(map[string]string, len(toolUses))
	for _, tu := range toolUses {
		matchedTaskIDs[tu.ID] = lookupMatchedTaskID(ctx, cfg.AgentContext, cfg.AuthorizationContext, cfg.RewriteContext, tu)
	}
	emitAuditEvents(ctx, result, toolUses, cfg.Inspector, matchedTaskIDs, emit)
	return evalFn
}

// emitAuditEvents walks the pipeline result's typed AuditEvent stream
// and emits one event per winning tool_use verdict. InspectorVerdict is
// re-derived from the supplied inspector; OutcomeName is derived from
// the verdict's typed Facts via conversation.OutcomeNameFromFacts;
// TaskID falls back to the matchedTaskIDs map when not surfaced by
// facts.
//
// Replaces the legacy policies.EmitToolUseAuditRows compat shim (Phase 9
// strict strip). The pipeline package owns the typed event stream; the
// emit helper is a pure translator inlined at the call site.
func emitAuditEvents(
	ctx context.Context,
	result *pipeline.ToolUseResult,
	toolUses []conversation.ToolUse,
	insp *inspector.Inspector,
	matchedTaskIDs map[string]string,
	emit func(conversation.AuditEvent),
) {
	if result == nil || emit == nil {
		return
	}
	events := result.AuditEvents(toolUses)
	factsByTU := make(map[string][]conversation.EvaluationFact, len(toolUses))
	for _, ev := range events {
		factsByTU[ev.ToolUse.ID] = append(factsByTU[ev.ToolUse.ID], ev.Facts...)
	}
	emitted := make(map[string]bool, len(toolUses))
	for _, ev := range events {
		if !ev.Winning || emitted[ev.ToolUse.ID] {
			continue
		}
		emitted[ev.ToolUse.ID] = true
		winningV := result.PerToolUse[ev.ToolUse.ID]
		out := conversation.AuditEvent{
			ToolUse:       ev.ToolUse,
			EvaluatorName: ev.EvaluatorName,
			Outcome:       ev.Outcome,
			Decision:      ev.Decision,
			Reason:        winningV.Reason,
			Facts:         ev.Facts,
			Winning:       true,
		}
		if out.Reason == "" {
			out.Reason = ev.Reason
		}
		if insp != nil {
			out.InspectorVerdict = llmproxy.InspectorSnapshot(insp.Inspect(ctx, inspector.ToolUse{
				ID:    ev.ToolUse.ID,
				Name:  ev.ToolUse.Name,
				Input: ev.ToolUse.Input,
			}))
		}
		out.OutcomeName = conversation.OutcomeNameFromFacts(ev.EvaluatorName, ev.Outcome, ev.Facts)
		out.TaskID = conversation.MatchedTaskIDFromFacts(factsByTU[ev.ToolUse.ID])
		if out.TaskID == "" {
			if id := matchedTaskIDs[ev.ToolUse.ID]; id != "" {
				out.TaskID = id
			}
		}
		emit(out)
	}
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
func (r *multiToolUseResponse) IsStreaming() bool                { return false }
func (r *multiToolUseResponse) ToolUses() []conversation.ToolUse { return r.toolUses }

func lookupMatchedTaskID(ctx context.Context, agent llmproxy.AgentContext, auth llmproxy.AuthorizationContext, rewrite llmproxy.RewriteContext, tu conversation.ToolUse) string {
	if rewrite.Inspector == nil {
		return ""
	}
	v := rewrite.Inspector.Inspect(ctx, inspector.ToolUse{
		ID:    tu.ID,
		Name:  tu.Name,
		Input: tu.Input,
	})
	if !v.IsAPICall || v.Ambiguous || v.Host == "" {
		return ""
	}
	var serviceID, actionID string
	if auth.Catalog != nil {
		if resolved, ok := auth.Catalog.Resolve(v.Host, v.Method, v.Path); ok {
			serviceID = resolved.ServiceID
			actionID = resolved.ActionID
		}
	}
	if auth.CandidateTasks == nil && auth.ToolRules == nil && auth.EgressRules == nil {
		if auth.TaskScope == nil || serviceID == "" || actionID == "" {
			return ""
		}
		dec := auth.TaskScope.Check(ctx, agent.AgentUserID, agent.AgentID, serviceID, actionID)
		return dec.TaskID
	}
	decisionInput := runtimedecision.AuthorizationInput{
		ToolUse:         tu,
		UserID:          agent.AgentUserID,
		AgentID:         agent.AgentID,
		Posture:         auth.Posture,
		Target:          runtimedecision.TargetRequest{Host: v.Host, Method: v.Method, Path: v.Path},
		Service:         serviceID,
		Action:          actionID,
		CandidateTasks:  auth.CandidateTasks,
		ToolRules:       auth.ToolRules,
		EgressRules:     auth.EgressRules,
		PreferredTaskID: auth.PreferredTaskID,
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

// Legacy singletonToolUseResponse + its methods have been removed —
// every postproc caller now supplies the full sibling set to the
// factory and the orchestrator runs response-level. multiToolUseResponse
// is the canonical pipeline-side response type.

func buildControlResolver(
	req *http.Request,
	agent llmproxy.AgentContext,
	audit llmproxy.AuditContext,
	approval llmproxy.ApprovalContext,
	rewrite llmproxy.RewriteContext,
	routing llmproxy.RoutingContext,
	provider conversation.Provider,
	emit func(conversation.AuditEvent),
) policies.ControlToolUseResolver {
	if routing.ControlBaseURL == "" {
		return nil
	}
	controlBaseURL := routing.ControlBaseURL
	agentID := agent.AgentID
	cache := rewrite.CallerNonces
	interceptCfg := llmproxy.PostprocessConfig{
		AgentContext:    agent,
		AuditContext:    audit,
		ApprovalContext: approval,
		RewriteContext:  rewrite,
		RoutingContext:  routing,
	}
	return func(_ context.Context, _ conversation.ToolUse) *policies.ControlToolUseInputs {
		return &policies.ControlToolUseInputs{
			ControlBaseURL: controlBaseURL,
			AgentID:        agentID,
			CallerNonces:   cache,
			InterceptInline: func(_ context.Context, tu conversation.ToolUse, call llmproxy.ControlCall) (pipeline.ToolUseVerdict, bool) {
				auditFn := func(decision, outcome, reason string) {
					emit(conversation.AuditEvent{
						ToolUse:     tu,
						Decision:    conversation.DecisionKind(decision),
						OutcomeName: outcome,
						Reason:      reason,
					})
				}
				traceFn := func(_ string, _ ...any) {}
				convV, claimed := llmproxy.MaybeInterceptInlineTaskDefinition(req, interceptCfg, auditFn, traceFn, provider, tu, call)
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
//  1. placeholder exists in the store
//  2. placeholder is owned by the calling agent
//  3. target host is in the placeholder's bound-service allowlist
//
// Each failure mode returns a distinct BoundaryDenyReason so audit
// rows tell operators WHICH check rejected the call instead of always
// reading "host not allowed."
//
// Without this defense-in-depth, an autovault placeholder belonging
// to a different agent could be sent to any host that looked like it
// accepted that credential; the resolver would still catch it at the
// network boundary, but the proxy's pre-flight check would have
// silently passed.
//
// Phase C narrowed signature: only AgentContext (identity) + the
// placeholder store flow in; no other sub-contexts are reachable.
func buildBoundaryResolver(agent llmproxy.AgentContext, st store.Store) policies.BoundaryResolver {
	if st == nil {
		return nil
	}
	userID := agent.AgentUserID
	agentID := agent.AgentID
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

// buildCredentialedTaskScope builds the credentialed-path authorization
// closure that TaskScopeEvaluator consumes via its TaskScopeResolver.
// The closure runs the runtimedecision.EvaluateAuthorization flow on
// the credentialed (host, method, path) target, handles Hold
// side-effects (PendingApprovals.Hold + ApprovalPrompt rendering +
// CleanupEvictedInlineTask), and emits audit rows. Returns an empty
// TaskScopeDecision when the call is authorized so TaskScopeEvaluator
// Skips and downstream stages (IntentVerify, CredentialRewrite) run.
//
// Inlined from the legacy EvaluateCredentialedAuthorization helper
// (Phase 6).
func buildCredentialedTaskScope(
	agent llmproxy.AgentContext,
	auditCtx llmproxy.AuditContext,
	auth llmproxy.AuthorizationContext,
	approval llmproxy.ApprovalContext,
	rewrite llmproxy.RewriteContext,
	provider conversation.Provider,
	emit func(conversation.AuditEvent),
) policies.TaskScopeResolver {
	if rewrite.Inspector == nil {
		return nil
	}
	approvalCleanupCfg := llmproxy.PostprocessConfig{ApprovalContext: approval}
	intentVerifyCfg := llmproxy.PostprocessConfig{AuthorizationContext: auth}
	return func(ctx context.Context, tu conversation.ToolUse) llmproxy.TaskScopeDecision {
		v := rewrite.Inspector.Inspect(ctx, inspector.ToolUse{
			ID:    tu.ID,
			Name:  tu.Name,
			Input: tu.Input,
		})
		if !v.IsAPICall || v.Ambiguous {
			return llmproxy.TaskScopeDecision{}
		}
		audit := func(decision, outcome, reason, taskID string) {
			if emit == nil {
				return
			}
			emit(conversation.AuditEvent{
				ToolUse:          tu,
				InspectorVerdict: llmproxy.InspectorSnapshot(v),
				Decision:         conversation.DecisionKind(decision),
				OutcomeName:      outcome,
				Reason:           reason,
				TaskID:           taskID,
			})
		}
		if auth.CandidateTasks != nil || auth.ToolRules != nil || auth.EgressRules != nil {
			resolved := llmproxy.ResolvedAction{}
			if auth.Catalog != nil {
				resolved, _ = auth.Catalog.Resolve(v.Host, v.Method, v.Path)
			}
			decisionInput := runtimedecision.AuthorizationInput{
				ToolUse:         tu,
				UserID:          agent.AgentUserID,
				AgentID:         agent.AgentID,
				Posture:         auth.Posture,
				Target:          runtimedecision.TargetRequest{Host: v.Host, Method: v.Method, Path: v.Path},
				Service:         resolved.ServiceID,
				Action:          resolved.ActionID,
				CandidateTasks:  auth.CandidateTasks,
				ToolRules:       auth.ToolRules,
				EgressRules:     auth.EgressRules,
				PreferredTaskID: auth.PreferredTaskID,
				IntentVerifier:  llmproxy.DecisionIntentVerifierFor(auth.IntentVerifier),
			}
			dec, err := runtimedecision.EvaluateAuthorization(ctx, decisionInput)
			if err != nil {
				audit("block", "decision_error", err.Error(), "")
				return llmproxy.TaskScopeDecision{Allowed: false, Reason: "Clawvisor: authorization failed — " + err.Error()}
			}
			matchedTaskID := ""
			if dec.Task != nil {
				matchedTaskID = dec.Task.ID
			}
			switch dec.Kind {
			case runtimedecision.VerdictAllow:
				if dec.Task != nil && rewrite.Store != nil {
					_, _, _ = llmproxy.SlideTaskExpiry(ctx, rewrite.Store, dec.Task, time.Now().UTC())
				}
				return llmproxy.TaskScopeDecision{}
			case runtimedecision.VerdictDeny:
				audit("block", string(dec.Source), dec.Reason, matchedTaskID)
				return llmproxy.TaskScopeDecision{
					Allowed: false,
					Reason:  "Clawvisor: " + dec.Reason,
					TaskID:  matchedTaskID,
				}
			case runtimedecision.VerdictNeedsApproval:
				var approvalID string
				if approval.PendingApprovals != nil {
					held, herr := approval.PendingApprovals.Hold(ctx, llmproxy.PendingLiteApproval{
						UserID:         agent.AgentUserID,
						AgentID:        agent.AgentID,
						Provider:       provider,
						ConversationID: auditCtx.ConversationID,
						ToolUse:        tu,
						Inspector:      v,
						Fingerprint:    runtimedecision.Fingerprint(dec, decisionInput),
						Reason:         dec.Reason,
					})
					if herr != nil {
						audit("block", "approval_hold_error", herr.Error(), "")
						return llmproxy.TaskScopeDecision{Allowed: false, Reason: "Clawvisor: approval unavailable — " + herr.Error()}
					}
					if held.Evicted != nil {
						audit("block", "approval_evicted", "superseded pending approval "+held.Evicted.ID, "")
						llmproxy.CleanupEvictedInlineTask(ctx, approvalCleanupCfg, held.Evicted)
					}
					approvalID = held.Pending.ID
				}
				audit("block", string(dec.Source), dec.Reason, matchedTaskID)
				return llmproxy.TaskScopeDecision{
					Allowed: false,
					Reason:  "Clawvisor: approval required — " + dec.Reason,
					TaskID:  matchedTaskID,
				}
				_ = approvalID
			}
		}
		// Legacy TaskScope.Check + intent verify fallback.
		if auth.Catalog != nil && auth.TaskScope != nil {
			if resolved, ok := auth.Catalog.Resolve(v.Host, v.Method, v.Path); ok {
				dec := auth.TaskScope.Check(ctx, agent.AgentUserID, agent.AgentID, resolved.ServiceID, resolved.ActionID)
				if !dec.Allowed {
					audit("block", "task_scope_denied", dec.Reason, "")
					return llmproxy.TaskScopeDecision{
						Allowed: false,
						Reason:  "Clawvisor: no active task scope covers " + resolved.ServiceID + "." + resolved.ActionID + " — " + dec.Reason,
					}
				}
				if reason, ok := llmproxy.RunIntentVerify(ctx, intentVerifyCfg, dec, resolved, tu); !ok {
					audit("block", "intent_verification_failed", reason, dec.TaskID)
					return llmproxy.TaskScopeDecision{
						Allowed: false,
						Reason:  "Clawvisor: intent verification refused " + resolved.ServiceID + "." + resolved.ActionID + " — " + reason,
						TaskID:  dec.TaskID,
					}
				}
				if dec.MatchedTask != nil && rewrite.Store != nil {
					_, _, _ = llmproxy.SlideTaskExpiry(ctx, rewrite.Store, dec.MatchedTask, time.Now().UTC())
				}
				return llmproxy.TaskScopeDecision{}
			}
		}
		return llmproxy.TaskScopeDecision{}
	}
}

// buildAuthorizationResolver wires AuthorizationPolicy to
// PostprocessConfig's decision-engine inputs + PendingApprovals cache.
// Returns nil when the policy has no role (no inspector, no policy
// config, and no sensitive-path hook).
func buildAuthorizationResolver(
	agent llmproxy.AgentContext,
	audit llmproxy.AuditContext,
	auth llmproxy.AuthorizationContext,
	approval llmproxy.ApprovalContext,
	rewrite llmproxy.RewriteContext,
	provider conversation.Provider,
) policies.AuthorizationResolver {
	if rewrite.Inspector == nil {
		return nil
	}
	intentVerifier := llmproxy.DecisionIntentVerifierFor(auth.IntentVerifier)
	holdHandler := &authorizationHoldHandler{
		agent:    agent,
		audit:    audit,
		approval: approval,
		provider: provider,
	}
	slideTask := func(ctx context.Context, task *store.Task) {
		if rewrite.Store == nil || task == nil {
			return
		}
		_, _, _ = llmproxy.SlideTaskExpiry(ctx, rewrite.Store, task, time.Now().UTC())
	}
	return func(ctx context.Context, tu conversation.ToolUse, v inspector.Verdict) *policies.AuthorizationInputs {
		hasPolicyConfig := auth.CandidateTasks != nil || auth.ToolRules != nil || auth.EgressRules != nil
		readOnlyShell, sensitivePath := detectShellSpecials(tu, agent, auth)
		return &policies.AuthorizationInputs{
			Input: runtimedecision.AuthorizationInput{
				ToolUse:         tu,
				UserID:          agent.AgentUserID,
				AgentID:         agent.AgentID,
				Posture:         auth.Posture,
				CandidateTasks:  auth.CandidateTasks,
				ToolRules:       auth.ToolRules,
				EgressRules:     auth.EgressRules,
				PreferredTaskID: auth.PreferredTaskID,
				IntentVerifier:  intentVerifier,
			},
			HasPolicyConfig:      hasPolicyConfig,
			ShellSensitivePath:   sensitivePath,
			ReadOnlyShellCommand: readOnlyShell,
			HoldHandler:          holdHandler,
			SlideTask:            slideTask,
		}
	}
}

// detectShellSpecials replays the legacy read-only-shell + sensitive-
// path detection from EvaluateTriggerMissAuthorization so the resolver
// hands AuthorizationPolicy the right flags. Returns
// (readOnlyShellCommand, sensitivePath); when sensitivePath is true
// readOnlyShellCommand is forced false (sensitive overrides).
func detectShellSpecials(tu conversation.ToolUse, agent llmproxy.AgentContext, auth llmproxy.AuthorizationContext) (bool, bool) {
	if !toolnames.IsShellToolName(tu.Name) {
		return false, false
	}
	if !llmproxy.ReadOnlyShellCommandsAllowed(tu.Name, agent.AgentID, auth.ToolRules) {
		return false, false
	}
	cmd := llmproxy.ShellCommandFromInput(tu.Input)
	if cmd == "" {
		return false, false
	}
	readOnly, _ := inspector.IsReadOnlyBashCommand(cmd)
	if toolnames.SensitiveFileGuardEnabled(tu.Name, agent.AgentID, auth.ToolRules) {
		if _, _, hit := inspector.CommandReferencesSensitivePath(cmd); hit {
			return false, true
		}
	}
	return readOnly, false
}

// authorizationHoldHandler implements policies.AuthorizationHoldHandler
// for AuthorizationPolicy's approval flow. Commits the hold via
// PendingApprovals.Hold, renders the approval prompt with the
// resulting approval ID, and cleans up any evicted inline task.
type authorizationHoldHandler struct {
	agent    llmproxy.AgentContext
	audit    llmproxy.AuditContext
	approval llmproxy.ApprovalContext
	provider conversation.Provider
}

func (h *authorizationHoldHandler) Hold(ctx context.Context, req policies.AuthorizationHoldRequest) (policies.AuthorizationHoldResult, error) {
	if h.approval.PendingApprovals == nil {
		// Fail closed in the policy.
		return policies.AuthorizationHoldResult{Err: "approval cache not configured"}, nil
	}
	held, err := h.approval.PendingApprovals.Hold(ctx, llmproxy.PendingLiteApproval{
		UserID:         h.agent.AgentUserID,
		AgentID:        h.agent.AgentID,
		Provider:       h.provider,
		ConversationID: h.audit.ConversationID,
		ToolUse:        req.ToolUse,
		Inspector:      req.InspectorVerdict,
		Fingerprint:    runtimedecision.Fingerprint(req.Decision, req.Input),
		Reason:         req.Decision.Reason,
	})
	if err != nil {
		return policies.AuthorizationHoldResult{Err: err.Error()}, nil
	}
	approvalID := held.Pending.ID
	if held.Evicted != nil {
		llmproxy.CleanupEvictedInlineTask(ctx, llmproxy.PostprocessConfig{ApprovalContext: h.approval}, held.Evicted)
	}
	return policies.AuthorizationHoldResult{
		ApprovalID:     approvalID,
		SubstituteText: llmproxy.ApprovalPrompt(req.ToolUse, req.Decision.Reason, approvalID),
	}, nil
}

// buildReadOnlyShellResolver wires ReadOnlyShellPassthroughPolicy +
// SensitivePathPolicy to AgentID + ToolRules.
//
// Phase C narrowed signature: AgentContext supplies identity, the
// AuthorizationContext supplies the rule set. Nothing else is
// reachable from this builder's scope.
func buildReadOnlyShellResolver(agent llmproxy.AgentContext, auth llmproxy.AuthorizationContext) policies.ReadOnlyShellResolver {
	agentID := agent.AgentID
	toolRules := auth.ToolRules
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

// buildScriptSessionResolver pins the resolver to the proxy's
// /api/proxy mount so the policy can recognize already-rewritten
// script-session curls.
//
// Phase C narrowed signature: RewriteContext supplies RewriteOpts;
// nothing else is reachable from this builder's scope.
func buildScriptSessionResolver(rewrite llmproxy.RewriteContext) policies.ScriptSessionResolver {
	if rewrite.RewriteOpts.ResolverBaseURL == "" {
		return nil
	}
	resolverBaseURL := rewrite.RewriteOpts.ResolverBaseURL
	return func(_ context.Context, _ conversation.ToolUse) *policies.ScriptSessionInputs {
		return &policies.ScriptSessionInputs{ResolverBaseURL: resolverBaseURL}
	}
}

// buildRewriteResolver wires CredentialRewriteEvaluator to the
// inspector + nonce cache + rewrite opts that the rewrite stage
// needs.
//
// Phase C narrowed signature: AgentContext supplies identity,
// RewriteContext supplies Inspector + CallerNonces + RewriteOpts.
func buildRewriteResolver(agent llmproxy.AgentContext, rewrite llmproxy.RewriteContext) policies.CredentialRewriteResolver {
	if rewrite.Inspector == nil {
		return nil
	}
	insp := rewrite.Inspector
	opts := rewrite.RewriteOpts
	cache := rewrite.CallerNonces
	agentID := agent.AgentID
	return func(_ context.Context, _ conversation.ToolUse) *policies.CredentialRewriteInputs {
		return &policies.CredentialRewriteInputs{
			Inspector:    insp,
			CallerNonces: cache,
			AgentID:      agentID,
			RewriteOpts:  opts,
		}
	}
}
