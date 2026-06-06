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
	credentialedTaskScope := buildCredentialedTaskScope(cfg, provider, emit)

	chain := policies.ComposeToolUseEvaluatorChain(policies.ToolUseChainConfig{
		Control:       buildControlResolver(req, cfg, provider, emit),
		ScriptSession: buildScriptSessionResolver(cfg),
		Inspector:     cfg.Inspector,
		Boundary:      buildBoundaryResolver(cfg),
		ReadOnlyShell: buildReadOnlyShellResolver(cfg),
		Authorization: buildAuthorizationResolver(cfg, provider),
		TaskScope:     credentialedTaskScope,
		Rewrite:       buildRewriteResolver(cfg),
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
		matchedTaskIDs[tu.ID] = lookupMatchedTaskID(ctx, cfg, tu)
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

// Legacy singletonToolUseResponse + its methods have been removed —
// every postproc caller now supplies the full sibling set to the
// factory and the orchestrator runs response-level. multiToolUseResponse
// is the canonical pipeline-side response type.

func buildControlResolver(req *http.Request, cfg llmproxy.PostprocessConfig, provider conversation.Provider, emit func(conversation.AuditEvent)) policies.ControlToolUseResolver {
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
					emit(conversation.AuditEvent{
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
func buildCredentialedTaskScope(cfg llmproxy.PostprocessConfig, provider conversation.Provider, emit func(conversation.AuditEvent)) policies.TaskScopeResolver {
	return func(ctx context.Context, tu conversation.ToolUse) llmproxy.TaskScopeDecision {
		v := cfg.Inspector.Inspect(ctx, inspector.ToolUse{
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
		if cfg.CandidateTasks != nil || cfg.ToolRules != nil || cfg.EgressRules != nil {
			resolved := llmproxy.ResolvedAction{}
			if cfg.Catalog != nil {
				resolved, _ = cfg.Catalog.Resolve(v.Host, v.Method, v.Path)
			}
			decisionInput := runtimedecision.AuthorizationInput{
				ToolUse:         tu,
				UserID:          cfg.AgentUserID,
				AgentID:         cfg.AgentID,
				Posture:         cfg.Posture,
				Target:          runtimedecision.TargetRequest{Host: v.Host, Method: v.Method, Path: v.Path},
				Service:         resolved.ServiceID,
				Action:          resolved.ActionID,
				CandidateTasks:  cfg.CandidateTasks,
				ToolRules:       cfg.ToolRules,
				EgressRules:     cfg.EgressRules,
				PreferredTaskID: cfg.PreferredTaskID,
				IntentVerifier:  llmproxy.DecisionIntentVerifierFor(cfg.IntentVerifier),
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
				if dec.Task != nil && cfg.Store != nil {
					_, _, _ = llmproxy.SlideTaskExpiry(ctx, cfg.Store, dec.Task, time.Now().UTC())
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
				if cfg.PendingApprovals != nil {
					held, herr := cfg.PendingApprovals.Hold(ctx, llmproxy.PendingLiteApproval{
						UserID:         cfg.AgentUserID,
						AgentID:        cfg.AgentID,
						Provider:       provider,
						ConversationID: cfg.ConversationID,
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
						llmproxy.CleanupEvictedInlineTask(ctx, cfg, held.Evicted)
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
		if cfg.Catalog != nil && cfg.TaskScope != nil {
			if resolved, ok := cfg.Catalog.Resolve(v.Host, v.Method, v.Path); ok {
				dec := cfg.TaskScope.Check(ctx, cfg.AgentUserID, cfg.AgentID, resolved.ServiceID, resolved.ActionID)
				if !dec.Allowed {
					audit("block", "task_scope_denied", dec.Reason, "")
					return llmproxy.TaskScopeDecision{
						Allowed: false,
						Reason:  "Clawvisor: no active task scope covers " + resolved.ServiceID + "." + resolved.ActionID + " — " + dec.Reason,
					}
				}
				if reason, ok := llmproxy.RunIntentVerify(ctx, cfg, dec, resolved, tu); !ok {
					audit("block", "intent_verification_failed", reason, dec.TaskID)
					return llmproxy.TaskScopeDecision{
						Allowed: false,
						Reason:  "Clawvisor: intent verification refused " + resolved.ServiceID + "." + resolved.ActionID + " — " + reason,
						TaskID:  dec.TaskID,
					}
				}
				if dec.MatchedTask != nil && cfg.Store != nil {
					_, _, _ = llmproxy.SlideTaskExpiry(ctx, cfg.Store, dec.MatchedTask, time.Now().UTC())
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
func buildAuthorizationResolver(cfg llmproxy.PostprocessConfig, provider conversation.Provider) policies.AuthorizationResolver {
	if cfg.Inspector == nil {
		return nil
	}
	intentVerifier := llmproxy.DecisionIntentVerifierFor(cfg.IntentVerifier)
	holdHandler := &authorizationHoldHandler{
		cfg:      cfg,
		provider: provider,
	}
	slideTask := func(ctx context.Context, task *store.Task) {
		if cfg.Store == nil || task == nil {
			return
		}
		_, _, _ = llmproxy.SlideTaskExpiry(ctx, cfg.Store, task, time.Now().UTC())
	}
	return func(ctx context.Context, tu conversation.ToolUse, v inspector.Verdict) *policies.AuthorizationInputs {
		hasPolicyConfig := cfg.CandidateTasks != nil || cfg.ToolRules != nil || cfg.EgressRules != nil
		readOnlyShell, sensitivePath := detectShellSpecials(tu, cfg)
		return &policies.AuthorizationInputs{
			Input: runtimedecision.AuthorizationInput{
				ToolUse:         tu,
				UserID:          cfg.AgentUserID,
				AgentID:         cfg.AgentID,
				Posture:         cfg.Posture,
				CandidateTasks:  cfg.CandidateTasks,
				ToolRules:       cfg.ToolRules,
				EgressRules:     cfg.EgressRules,
				PreferredTaskID: cfg.PreferredTaskID,
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
func detectShellSpecials(tu conversation.ToolUse, cfg llmproxy.PostprocessConfig) (bool, bool) {
	if !toolnames.IsShellToolName(tu.Name) {
		return false, false
	}
	if !llmproxy.ReadOnlyShellCommandsAllowed(tu.Name, cfg.AgentID, cfg.ToolRules) {
		return false, false
	}
	cmd := llmproxy.ShellCommandFromInput(tu.Input)
	if cmd == "" {
		return false, false
	}
	readOnly, _ := inspector.IsReadOnlyBashCommand(cmd)
	if toolnames.SensitiveFileGuardEnabled(tu.Name, cfg.AgentID, cfg.ToolRules) {
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
	cfg      llmproxy.PostprocessConfig
	provider conversation.Provider
}

func (h *authorizationHoldHandler) Hold(ctx context.Context, req policies.AuthorizationHoldRequest) (policies.AuthorizationHoldResult, error) {
	if h.cfg.PendingApprovals == nil {
		// Fail closed in the policy.
		return policies.AuthorizationHoldResult{Err: "approval cache not configured"}, nil
	}
	held, err := h.cfg.PendingApprovals.Hold(ctx, llmproxy.PendingLiteApproval{
		UserID:         h.cfg.AgentUserID,
		AgentID:        h.cfg.AgentID,
		Provider:       h.provider,
		ConversationID: h.cfg.ConversationID,
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
		llmproxy.CleanupEvictedInlineTask(ctx, h.cfg, held.Evicted)
	}
	return policies.AuthorizationHoldResult{
		ApprovalID:     approvalID,
		SubstituteText: llmproxy.ApprovalPrompt(req.ToolUse, req.Decision.Reason, approvalID),
	}, nil
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
