// Package pipelineeval exposes the policies-chain-based
// llmproxy.ToolUseEvaluatorFactory as Factory. It is the adapter layer
// between llmproxy's response config and the policy / pipeline packages,
// so handlers and llmproxy's own tests can share the same evaluator
// construction without introducing a policies -> llmproxy import cycle.
//
// The factory composes the seven-stage chain (ControlToolUseEvaluator
// + ScriptSessionEvaluator + AuthorizationPolicy + InspectorChain with
// TriggerMissAuthorizer + TaskScopeEvaluator + IntentVerifyEvaluator +
// CredentialRewriteEvaluator)
// and runs the whole sibling tool_use set once through
// pipeline.RunToolUseEvaluators. The returned evaluator is a verdict
// lookup; audit rows and buffered holds are emitted through the typed
// conversation.AuditEvent callback the caller supplies.
package pipelineeval

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/approvaltext"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/controltool"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/inspector"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/intentverify"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/pipeline"
	placeholderpkg "github.com/clawvisor/clawvisor/internal/runtime/llmproxy/placeholder"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/policies"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/shellpolicy"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/tasklifetime"
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
	credentialedBundle := buildCredentialedTaskScope(
		cfg.AgentContext,
		cfg.AuditContext,
		cfg.AuthorizationContext,
		cfg.ApprovalContext,
		cfg.RewriteContext,
		cfg.RoutingContext,
		provider,
		emit,
	)

	authBundle := buildAuthorizationResolver(cfg.AgentContext, cfg.AuditContext, cfg.AuthorizationContext, cfg.ApprovalContext, cfg.RewriteContext, cfg.RoutingContext, provider)

	chain := policies.ComposeToolUseEvaluatorChain(policies.ToolUseChainConfig{
		Control:       buildControlResolver(req, cfg.AgentContext, cfg.AuditContext, cfg.ApprovalContext, cfg.RewriteContext, cfg.RoutingContext, provider, emit),
		ScriptSession: buildScriptSessionResolver(cfg.RewriteContext, cfg.ScriptSessionContext),
		Inspector:     cfg.Inspector,
		Boundary:      buildBoundaryResolver(cfg.AgentContext, cfg.Store),
		Authorization: authBundle.Resolve,
		TaskScope:     credentialedBundle.Resolve,
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
	// Prime the authorization decision caches by batch-evaluating all
	// sibling tool_uses' decision-engine inputs in parallel before the
	// orchestrator's serial loop starts. This collapses N intent-verifier
	// round-trips per turn (one per parallel tool_use) into a single
	// wall-clock round-trip per path — the trigger-miss
	// (AuthorizationPolicy) and credentialed (TaskScopeEvaluator) paths
	// each maintain their own cache and run their batch independently.
	// Per-tool-use side effects (audit emission, PendingApprovals.Hold,
	// SlideTaskExpiry) still run serially inside the orchestrator's
	// existing loop so approval-hold ordering and audit ordering are
	// preserved.
	//
	// The two prefetches run sequentially, not concurrently. Both walk
	// rewrite.Inspector over the full sibling set, and Inspector +
	// Validator implementations don't document a concurrent-Inspect
	// contract — wrapping an unsynchronized backend would race. The
	// verifier calls INSIDE each batch still fan out via the decision
	// engine's EvaluateAuthorizationBatch (which already requires
	// IntentVerifier to be concurrency-safe), so the per-path wins are
	// preserved. The only cost is the rare turn that mixes both paths,
	// which pays two batch latencies instead of one.
	if authBundle.Prefetch != nil {
		authBundle.Prefetch(ctx, toolUses)
	}
	if credentialedBundle.Prefetch != nil {
		credentialedBundle.Prefetch(ctx, toolUses)
	}
	res := &multiToolUseResponse{provider: provider, toolUses: toolUses}
	evalFn, result, err := pipeline.RunToolUseEvaluators(ctx, res, toolUses, chain)
	if err != nil {
		// Pipeline errored before producing per-tool verdicts. Emit
		// one audit row per sibling so every blocked tool_use has an
		// investigable row.
		for _, tu := range toolUses {
			emit(conversation.AuditEvent{
				ToolUse:     tu,
				Decision:    conversation.DecisionBlock,
				OutcomeName: "pipeline_error",
				Reason:      err.Error(),
			})
		}
		errMsg := policies.ModelSafeInternalReason("authorization pipeline")
		return func(_ conversation.ToolUse) conversation.ToolUseVerdict {
			return conversation.ToolUseVerdict{Allowed: false, Reason: errMsg}
		}
	}
	// Emit one audit row per tool_use. Evaluators report observations
	// through the typed pipeline result; no side-channel audit emission
	// is used for the winning verdicts here.
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
// The pipeline package owns the typed event stream; this helper is a
// pure translator at the llmproxy adapter boundary.
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
	annotationsByTU := make(map[string][]conversation.EvaluationFact, len(toolUses))
	for _, ev := range events {
		factsByTU[ev.ToolUse.ID] = append(factsByTU[ev.ToolUse.ID], ev.Facts...)
		// Non-winning evaluations carry forensic facts (e.g. judge
		// invocation cost from a script-session Skip) that should
		// still reach the audit row. Capture them into a separate
		// bucket so the OutcomeName lookup (which uses winning-only
		// ev.Facts) doesn't pick up the early-stage outcome string
		// by accident. MatchedTaskID lookup deliberately walks the
		// full factsByTU below — a non-winning task-scope evaluator
		// may have matched the task even if a later stage produced
		// the winning verdict.
		if !ev.Winning && len(ev.Facts) > 0 {
			annotationsByTU[ev.ToolUse.ID] = append(annotationsByTU[ev.ToolUse.ID], ev.Facts...)
		}
	}
	emitted := make(map[string]bool, len(toolUses))
	for _, ev := range events {
		if !ev.Winning || emitted[ev.ToolUse.ID] {
			continue
		}
		emitted[ev.ToolUse.ID] = true
		winningV := result.PerToolUse[ev.ToolUse.ID]
		out := conversation.AuditEvent{
			ToolUse:         ev.ToolUse,
			EvaluatorName:   ev.EvaluatorName,
			Outcome:         ev.Outcome,
			Decision:        ev.Decision,
			Reason:          winningV.Reason,
			Facts:           ev.Facts,
			AnnotationFacts: annotationsByTU[ev.ToolUse.ID],
			Winning:         true,
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

// Every postproc caller supplies the full sibling set to the factory,
// and the orchestrator runs response-level. multiToolUseResponse is the
// canonical pipeline-side response type.

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
			InterceptInline: func(_ context.Context, tu conversation.ToolUse, call controltool.ControlCall) (pipeline.ToolUseVerdict, bool) {
				auditFn := func(decision, outcome, reason string) {
					emit(conversation.AuditEvent{
						ToolUse:     tu,
						Decision:    conversation.DecisionKind(decision),
						OutcomeName: outcome,
						Reason:      reason,
					})
				}
				// Forward trace events through the real TraceLogger
				// when one is configured. Previous stub callback
				// silently dropped events like inline_task.held and
				// inline_task.substitution_shape — invisible to
				// operators trying to debug whether the AskUserQuestion
				// substitution fired. Also serves the expansion
				// intercept's tracing (inline_expansion.held etc.).
				traceFn := func(event string, kv ...any) {
					if audit.Trace == nil {
						return
					}
					payload := map[string]any{
						"event":           event,
						"agent_id":        agentID,
						"request_id":      audit.RequestID,
						"conversation_id": audit.ConversationID,
						"tool_use_id":     tu.ID,
					}
					for i := 0; i+1 < len(kv); i += 2 {
						key, ok := kv[i].(string)
						if !ok {
							continue
						}
						payload[key] = kv[i+1]
					}
					audit.Trace.Emit(payload)
				}
				// Scope-drift one-off intercept runs first: its path
				// (/api/control/scope-drifts/<id>/one-off) doesn't
				// collide with the task / expand prefixes, so order
				// here is by specificity rather than necessity.
				if convV, claimed := llmproxy.MaybeInterceptScopeDriftOneOff(req, interceptCfg, auditFn, traceFn, provider, tu, call); claimed {
					return conversationToPipelineVerdict(convV), true
				}
				// Expansion intercept runs next: it matches a
				// strict-prefix path (.../tasks/<id>/expand) so it
				// can't claim a creation POST (.../tasks). If it
				// passes, the creation intercept gets its turn; if
				// neither claims, fall through to the regular
				// control-rewrite path.
				if convV, claimed := llmproxy.MaybeInterceptInlineExpansion(req, interceptCfg, auditFn, traceFn, provider, tu, call); claimed {
					return conversationToPipelineVerdict(convV), true
				}
				convV, claimed := llmproxy.MaybeInterceptInlineTaskDefinition(req, interceptCfg, auditFn, traceFn, provider, tu, call)
				if !claimed {
					return pipeline.ToolUseVerdict{}, false
				}
				return conversationToPipelineVerdict(convV), true
			},
		}
	}
}

// conversationToPipelineVerdict normalizes helper verdicts that only set
// Allowed. The verdict type is unified, so the only field-level
// translation needed is deriving Outcome from Allowed.
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
		if v.Continue == nil && (v.HeldKindHint == pipeline.HeldKindHintApproval || v.HoldKey != "" || v.SubstituteWith != "") {
			v.Outcome = pipeline.OutcomeHold
		} else {
			v.Outcome = pipeline.OutcomeDeny
		}
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
// The narrowed signature keeps this builder limited to identity and the
// placeholder store; no other postprocess sub-contexts are reachable.
func buildBoundaryResolver(agent llmproxy.AgentContext, st store.Store) policies.BoundaryResolver {
	if st == nil {
		return nil
	}
	userID := agent.AgentUserID
	agentID := agent.AgentID
	return func(ctx context.Context, v inspector.Verdict) policies.BoundaryDecision {
		if len(v.Placeholders) == 0 {
			return policies.BoundaryDecision{Allowed: true}
		}
		var commonHosts map[string]struct{}
		for _, placeholder := range v.Placeholders {
			rec, err := st.GetRuntimePlaceholder(ctx, placeholder)
			if err != nil || rec == nil {
				return policies.BoundaryDecision{
					DenyReason: pipeline.BoundaryDenyReasonPlaceholderUnknown,
					Reason:     "Clawvisor: autovault placeholder not found in store",
				}
			}
			if _, ok := placeholderpkg.ValidateRuntimePlaceholderAccess(ctx, st, rec, userID, agentID, time.Now().UTC()); !ok {
				return policies.BoundaryDecision{
					DenyReason: pipeline.BoundaryDenyReasonOwnershipMismatch,
					Reason:     "Clawvisor: autovault placeholder belongs to a different agent",
				}
			}
			hosts, hostReason := placeholderpkg.RuntimePlaceholderBoundHosts(ctx, st, rec)
			if len(hosts) == 0 {
				if hostReason == "" {
					hostReason = "Clawvisor: credential grant host lookup failed"
				}
				return policies.BoundaryDecision{
					DenyReason: pipeline.BoundaryDenyReasonHostNotAllowed,
					Reason:     hostReason,
				}
			}
			if commonHosts == nil {
				commonHosts = make(map[string]struct{}, len(hosts))
				for _, host := range hosts {
					commonHosts[host] = struct{}{}
				}
				continue
			}
			nextCommonHosts := make(map[string]struct{}, len(commonHosts))
			for _, host := range hosts {
				if _, ok := commonHosts[host]; ok {
					nextCommonHosts[host] = struct{}{}
				}
			}
			commonHosts = nextCommonHosts
			if len(commonHosts) == 0 {
				return policies.BoundaryDecision{
					DenyReason: pipeline.BoundaryDenyReasonHostNotAllowed,
					Reason:     "Clawvisor: no shared host allowlist covers every autovault placeholder",
				}
			}
		}
		hosts := make([]string, 0, len(commonHosts))
		for host := range commonHosts {
			hosts = append(hosts, host)
		}
		ok, reason := inspector.BoundaryCheck(v, hosts)
		decision := policies.BoundaryDecision{Allowed: ok, AllowedHosts: hosts, Reason: reason}
		if !ok {
			decision.DenyReason = pipeline.BoundaryDenyReasonHostNotAllowed
		}
		return decision
	}
}

// credentialedTaskScopeBundle pairs the TaskScopeResolver
// TaskScopeEvaluator consumes with an optional Prefetch hook the
// Factory invokes once per response to batch the credentialed-path
// decision-engine calls for every sibling tool_use in parallel.
//
// Prefetch is best-effort and only covers the modern decision-engine
// path (auth.CandidateTasks / ToolRules / EgressRules set). The
// legacy TaskScope.Check + runIntentVerify fallback stays serial: it
// is a deprecated path that few callers exercise, and the side
// effects there are interleaved with the verifier call in a way that
// does not factor as cleanly.
type credentialedTaskScopeBundle struct {
	Resolve  policies.TaskScopeResolver
	Prefetch func(ctx context.Context, toolUses []conversation.ToolUse)
}

// credentialedPlan captures everything the credentialed-path resolver
// needs to apply side effects after a decision has been computed: the
// inspector verdict + catalog resolution that drove the input, plus
// the decision-engine input itself (required for Fingerprint on hold).
type credentialedPlan struct {
	Verdict       inspector.Verdict
	Resolved      llmproxy.ResolvedAction
	DecisionInput runtimedecision.AuthorizationInput
}

// credentialedOutcome is what Prefetch stashes in the per-response
// cache: the planning context plus the batched decision outcome.
type credentialedOutcome struct {
	Plan    credentialedPlan
	Outcome runtimedecision.AuthorizationOutcome
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
// Prefetch (returned alongside Resolve) batches the modern-path
// EvaluateAuthorization calls across all sibling tool_uses so a turn
// with N parallel credentialed tool_uses pays one verifier round-trip
// of wall time, not N. Side effects (audit / Hold / SlideTaskExpiry)
// still fire serially inside Resolve so hold-eviction ordering and
// audit ordering are unchanged.
func buildCredentialedTaskScope(
	agent llmproxy.AgentContext,
	auditCtx llmproxy.AuditContext,
	auth llmproxy.AuthorizationContext,
	approval llmproxy.ApprovalContext,
	rewrite llmproxy.RewriteContext,
	routing llmproxy.RoutingContext,
	provider conversation.Provider,
	emit func(conversation.AuditEvent),
) credentialedTaskScopeBundle {
	if rewrite.Inspector == nil {
		return credentialedTaskScopeBundle{}
	}
	approvalCleanupCfg := llmproxy.PostprocessConfig{ApprovalContext: approval}
	// Coordinator owns the scope-drift integration both branches below
	// route through (modern decision mint + retry pre-clear consume).
	// Nil-tolerant: when ScopeDrifts isn't wired, every method short-
	// circuits and the closures below fall through to their legacy
	// paths unchanged.
	drifts := newScopeDriftCoordinator(agent, auditCtx, auth, routing, provider)

	// planModernPath builds the per-tool-use credentialedPlan for the
	// modern decision-engine path. Returns (plan, true) when the tool_use
	// is credentialed AND the modern path is configured. Returns
	// (_, false) when the legacy fallback should handle it (or the
	// tool_use is not credentialed at all).
	planModernPath := func(ctx context.Context, tu conversation.ToolUse) (credentialedPlan, bool) {
		v := rewrite.Inspector.Inspect(ctx, inspector.ToolUse{
			ID:    tu.ID,
			Name:  tu.Name,
			Input: tu.Input,
		})
		if !v.IsAPICall || v.Ambiguous {
			return credentialedPlan{}, false
		}
		if auth.CandidateTasks == nil && auth.ToolRules == nil && auth.EgressRules == nil {
			return credentialedPlan{Verdict: v}, false
		}
		resolved := llmproxy.ResolvedAction{}
		if auth.Catalog != nil {
			resolved, _ = auth.Catalog.Resolve(v.Host, v.Method, v.Path)
		}
		return credentialedPlan{
			Verdict:  v,
			Resolved: resolved,
			DecisionInput: runtimedecision.AuthorizationInput{
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
				IntentVerifier:  intentverify.DecisionVerifierFor(auth.IntentVerifier),
			},
		}, true
	}

	// Per-response decision cache. Written by Prefetch after its
	// goroutines join; read by Resolve serially from the orchestrator
	// loop. No concurrent access.
	cache := make(map[string]credentialedOutcome)

	// applyModernDecision turns a (plan, decision, err) into the
	// TaskScopeDecision the policy consumes, firing audit / Hold /
	// SlideTaskExpiry side effects serially in tool_use order.
	applyModernDecision := func(
		ctx context.Context,
		tu conversation.ToolUse,
		plan credentialedPlan,
		dec runtimedecision.AuthorizationDecision,
		err error,
		audit func(decision, outcome, reason, taskID string),
	) policies.TaskScopeDecision {
		if err != nil {
			audit("block", "decision_error", err.Error(), "")
			return policies.TaskScopeDecision{Kind: policies.TaskScopeDecisionDeny, Reason: policies.ModelSafeInternalReason("authorization")}
		}
		matchedTaskID := ""
		if dec.Task != nil {
			matchedTaskID = dec.Task.ID
		}
		switch dec.Kind {
		case runtimedecision.VerdictAllow:
			if dec.Task != nil && rewrite.Store != nil {
				_, _, _ = tasklifetime.SlideTaskExpiry(ctx, rewrite.Store, dec.Task, time.Now().UTC())
			}
			return policies.TaskScopeDecision{}
		case runtimedecision.VerdictDeny:
			audit("block", string(dec.Source), dec.Reason, matchedTaskID)
			return policies.TaskScopeDecision{
				Kind:   policies.TaskScopeDecisionDeny,
				Reason: "Clawvisor: " + dec.Reason,
				TaskID: matchedTaskID,
			}
		case runtimedecision.VerdictNeedsApproval:
			// Scope drift: when the block is "no covering task scope",
			// redirect the agent to the continuation menu instead of
			// parking a user-facing approval prompt. Layer 2 hardcoded
			// approvals keep the existing user-prompt path.
			if mint := drifts.MintForCredentialed(ctx, tu, plan.Verdict, plan.Resolved, matchedTaskID, dec); mint.OK {
				audit("block", "scope_drift_minted", "drift_id="+mint.DriftID+"; "+dec.Reason, matchedTaskID)
				return policies.TaskScopeDecision{
					Kind:                   policies.TaskScopeDecisionHold,
					Allowed:                false,
					Reason:                 "Clawvisor: no active task scope covers " + plan.Resolved.ServiceID + "." + plan.Resolved.ActionID + " — " + dec.Reason,
					SubstituteText:         mint.MenuText,
					ContinueWithToolResult: mint.MenuText,
					TaskID:                 matchedTaskID,
				}
			} else if mint.Err != nil {
				// Registry write failed — fall through to the legacy
				// approval-hold path rather than emit a menu with no
				// usable drift_id. The audit row records the mint
				// failure so operators can investigate.
				audit("block", "scope_drift_mint_failed", mint.Err.Error(), matchedTaskID)
			}
			var approvalID string
			if approval.PendingApprovals != nil {
				held, herr := approval.PendingApprovals.Hold(ctx, llmproxy.PendingLiteApproval{
					UserID:         agent.AgentUserID,
					AgentID:        agent.AgentID,
					Provider:       provider,
					ConversationID: auditCtx.ConversationID,
					ToolUse:        tu,
					Inspector:      plan.Verdict,
					Fingerprint:    runtimedecision.Fingerprint(dec, plan.DecisionInput),
					Reason:         dec.Reason,
				})
				if herr != nil {
					audit("block", "approval_hold_error", herr.Error(), "")
					return policies.TaskScopeDecision{Kind: policies.TaskScopeDecisionDeny, Reason: policies.ModelSafeUnavailableReason("approval")}
				}
				if held.Evicted != nil {
					audit("block", "approval_evicted", "superseded pending approval "+held.Evicted.ID, "")
					llmproxy.CleanupEvictedInlineTask(ctx, approvalCleanupCfg, held.Evicted)
				}
				approvalID = held.Pending.ID
			}
			audit("block", string(dec.Source), dec.Reason, matchedTaskID)
			return policies.TaskScopeDecision{
				Kind:           policies.TaskScopeDecisionHold,
				Allowed:        false,
				Reason:         "Clawvisor: approval required — " + dec.Reason,
				SubstituteText: approvaltext.ApprovalPrompt(tu, dec.Reason, approvalID),
				TaskID:         matchedTaskID,
			}
		}
		return policies.TaskScopeDecision{}
	}

	resolve := func(ctx context.Context, tu conversation.ToolUse) policies.TaskScopeDecision {
		// Inspect once: needed for the not-credentialed early return,
		// the audit emitter's snapshot, and the legacy fallback branch.
		// Cheap and deterministic — duplicating the call against the
		// prefetch path is acceptable.
		v := rewrite.Inspector.Inspect(ctx, inspector.ToolUse{
			ID:    tu.ID,
			Name:  tu.Name,
			Input: tu.Input,
		})
		if !v.IsAPICall || v.Ambiguous {
			return policies.TaskScopeDecision{}
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
		// Scope-drift pre-clear: if the agent is retrying a tool_use the
		// user just approved (via expand / new_task / one_off), consume
		// the one-shot pre-clear so the retry passes scope check once
		// without re-running the full EvaluateAuthorization path. A
		// second retry of the same call lands back on the normal scope
		// path and either passes (if a covering task was created) or
		// mints a fresh drift.
		if driftID, hit := drifts.ConsumePreClear(ctx, tu, v); hit {
			audit("allow", "scope_drift_pre_clear", "drift_id="+driftID, "")
			return policies.TaskScopeDecision{Kind: policies.TaskScopeDecisionAllow, Allowed: true}
		}
		if auth.CandidateTasks != nil || auth.ToolRules != nil || auth.EgressRules != nil {
			if cached, ok := cache[tu.ID]; ok {
				return applyModernDecision(ctx, tu, cached.Plan, cached.Outcome.Decision, cached.Outcome.Err, audit)
			}
			// Cache miss (Prefetch not invoked, or this tool_use was
			// excluded from the batch). Fall back to inline evaluation.
			plan, planned := planModernPath(ctx, tu)
			if !planned {
				// planModernPath agreed it's credentialed but the modern
				// path isn't configured — fall through to legacy below.
			} else {
				dec, err := runtimedecision.EvaluateAuthorization(ctx, plan.DecisionInput)
				return applyModernDecision(ctx, tu, plan, dec, err, audit)
			}
		}
		// Legacy TaskScope.Check + intent verify fallback.
		if auth.TaskScope != nil {
			if auth.Catalog == nil {
				audit("block", "unresolved_action", "credentialed target catalog unavailable", "")
				return policies.TaskScopeDecision{Kind: policies.TaskScopeDecisionDeny, Reason: "Clawvisor: credentialed target could not be resolved"}
			}
			resolved, ok := auth.Catalog.Resolve(v.Host, v.Method, v.Path)
			if !ok {
				audit("block", "unresolved_action", "credentialed target not found in catalog", "")
				return policies.TaskScopeDecision{Kind: policies.TaskScopeDecisionDeny, Reason: "Clawvisor: credentialed target could not be resolved"}
			}
			dec := auth.TaskScope.Check(ctx, agent.AgentUserID, agent.AgentID, resolved.ServiceID, resolved.ActionID)
			if !dec.Allowed {
				// Route the legacy denial through the scope-drift menu
				// ONLY when the reason is a genuine scope miss
				// (needs_new_task / no_active_task). The other
				// non-Allowed reasons reported by StoreTaskScopeChecker
				// (task_store_unavailable, no_task_store_configured,
				// no_agent_context, unresolved_action,
				// unknown_classification) describe backend / config /
				// programmer errors — minting a drift for those would
				// let an agent ride out a degraded backend by picking
				// an option, landing a pre-clear, and bypassing the
				// (still broken) scope check on retry. Hard-block
				// those instead so an operator sees them.
				if isLegacyScopeDriftReason(dec.Reason) {
					syntheticDec := runtimedecision.AuthorizationDecision{
						Kind:   runtimedecision.VerdictNeedsApproval,
						Source: runtimedecision.SourceTaskScopeMissing,
						Reason: dec.Reason,
					}
					if mint := drifts.MintForCredentialed(ctx, tu, v, resolved, dec.TaskID, syntheticDec); mint.OK {
						audit("block", "scope_drift_minted", "drift_id="+mint.DriftID+"; "+dec.Reason+" (legacy)", dec.TaskID)
						return policies.TaskScopeDecision{
							Kind:                   policies.TaskScopeDecisionHold,
							Allowed:                false,
							Reason:                 "Clawvisor: no active task scope covers " + resolved.ServiceID + "." + resolved.ActionID + " — " + dec.Reason,
							SubstituteText:         mint.MenuText,
							ContinueWithToolResult: mint.MenuText,
							TaskID:                 dec.TaskID,
						}
					} else if mint.Err != nil {
						audit("block", "scope_drift_mint_failed", mint.Err.Error()+" (legacy)", dec.TaskID)
					}
				}
				audit("block", "task_scope_denied", dec.Reason, "")
				return policies.TaskScopeDecision{
					Kind:   policies.TaskScopeDecisionHold,
					Reason: "Clawvisor: no active task scope covers " + resolved.ServiceID + "." + resolved.ActionID + " — " + dec.Reason,
				}
			}
			if reason, ok := runIntentVerify(ctx, auth.IntentVerifier, dec, resolved, tu); !ok {
				audit("block", "intent_verification_failed", reason, dec.TaskID)
				return policies.TaskScopeDecision{
					Kind:   policies.TaskScopeDecisionDeny,
					Reason: "Clawvisor: intent verification refused " + resolved.ServiceID + "." + resolved.ActionID + " — " + reason,
					TaskID: dec.TaskID,
				}
			}
			if dec.MatchedTask != nil && rewrite.Store != nil {
				_, _, _ = tasklifetime.SlideTaskExpiry(ctx, rewrite.Store, dec.MatchedTask, time.Now().UTC())
			}
			return policies.TaskScopeDecision{}
		}
		return policies.TaskScopeDecision{}
	}

	prefetch := func(ctx context.Context, toolUses []conversation.ToolUse) {
		// Modern-path inputs only. Tool_uses that don't reach
		// EvaluateAuthorization (not credentialed, or legacy fallback)
		// are skipped — Resolve handles them inline.
		type pending struct {
			tuID string
			plan credentialedPlan
		}
		pendings := make([]pending, 0, len(toolUses))
		for _, tu := range toolUses {
			plan, planned := planModernPath(ctx, tu)
			if !planned {
				continue
			}
			pendings = append(pendings, pending{tuID: tu.ID, plan: plan})
		}
		if len(pendings) == 0 {
			return
		}
		batchInputs := make([]runtimedecision.AuthorizationInput, len(pendings))
		for i, p := range pendings {
			batchInputs[i] = p.plan.DecisionInput
		}
		outcomes := runtimedecision.EvaluateAuthorizationBatch(ctx, batchInputs)
		for i, out := range outcomes {
			cache[pendings[i].tuID] = credentialedOutcome{
				Plan:    pendings[i].plan,
				Outcome: out,
			}
		}
	}

	return credentialedTaskScopeBundle{Resolve: resolve, Prefetch: prefetch}
}

// authorizationResolverBundle pairs the AuthorizationResolver
// AuthorizationPolicy consumes with an optional Prefetch hook the
// Factory invokes once per response to batch the decision-engine
// calls for every sibling tool_use in parallel.
//
// Prefetch is best-effort: when the cache is primed, Resolve returns
// AuthorizationInputs with Precomputed set, and AuthorizationPolicy
// skips its inline EvaluateAuthorization call. When Prefetch was not
// invoked (or a tool_use was excluded from the batch), Resolve
// returns AuthorizationInputs without Precomputed and the policy
// falls back to the inline call. Either way, the side-effect dispatch
// (SlideTask / HoldHandler) still runs serially inside Evaluate so
// approval-hold ordering and audit emission stay identical.
type authorizationResolverBundle struct {
	Resolve  policies.AuthorizationResolver
	Prefetch func(ctx context.Context, toolUses []conversation.ToolUse)
}

// buildAuthorizationResolver wires AuthorizationPolicy to
// PostprocessConfig's decision-engine inputs + PendingApprovals cache.
// Returns an empty bundle when the policy has no role (no inspector
// configured).
func buildAuthorizationResolver(
	agent llmproxy.AgentContext,
	audit llmproxy.AuditContext,
	auth llmproxy.AuthorizationContext,
	approval llmproxy.ApprovalContext,
	rewrite llmproxy.RewriteContext,
	routing llmproxy.RoutingContext,
	provider conversation.Provider,
) authorizationResolverBundle {
	if rewrite.Inspector == nil {
		return authorizationResolverBundle{}
	}
	intentVerifier := intentverify.DecisionVerifierFor(auth.IntentVerifier)
	// One coordinator shared between Hold (mint side) and resolve
	// (pre-clear consume side) so an approved one-off and its retry
	// land against the same registry view.
	drifts := newScopeDriftCoordinator(agent, audit, auth, routing, provider)
	holdHandler := &authorizationHoldHandler{
		agent:    agent,
		audit:    audit,
		approval: approval,
		provider: provider,
		drifts:   drifts,
	}
	slideTask := func(ctx context.Context, task *store.Task) {
		if rewrite.Store == nil || task == nil {
			return
		}
		_, _, _ = tasklifetime.SlideTaskExpiry(ctx, rewrite.Store, task, time.Now().UTC())
	}

	// planFor builds the per-tool-use AuthorizationInputs the policy
	// consumes. Shared by Resolve (per-tool-use) and Prefetch
	// (response-scoped batch). Pure: no decision-engine call, no side
	// effects.
	planFor := func(tu conversation.ToolUse) *policies.AuthorizationInputs {
		hasPolicyConfig := auth.CandidateTasks != nil || auth.ToolRules != nil || auth.EgressRules != nil
		readOnlyShell, sensitivePath := detectShellSpecials(tu, agent, auth)
		shellPoll := shellpolicy.IsShellPollTool(tu.Name, tu.Input)
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
			ShellPoll:            shellPoll,
			HoldHandler:          holdHandler,
			SlideTask:            slideTask,
		}
	}

	// Per-response decision cache. Written by Prefetch (after its
	// goroutines join), read by Resolve from the orchestrator's
	// serial Evaluate loop. No concurrent access — Prefetch returns
	// before the orchestrator starts iterating.
	cache := make(map[string]runtimedecision.AuthorizationOutcome)

	resolve := func(ctx context.Context, tu conversation.ToolUse, v inspector.Verdict) *policies.AuthorizationInputs {
		inputs := planFor(tu)
		// Scope-drift pre-clear (trigger-miss path): if the agent is
		// retrying a tool_use after a user-approved one-off (or any
		// approval that landed a pre-clear), let it through once
		// without re-running EvaluateAuthorization. Without this the
		// authorization engine sees the same task_scope_missing source
		// and mints another drift, looping the agent through the menu.
		// We precompute a VerdictAllow so AuthorizationPolicy emits its
		// normal allow audit; the Reason carries the drift_id for
		// operator correlation.
		if driftID, hit := drifts.ConsumePreClearForTriggerMiss(ctx, tu, v); hit {
			allow := runtimedecision.AuthorizationDecision{
				Kind:   runtimedecision.VerdictAllow,
				Source: runtimedecision.SourceTaskScope,
				Reason: "scope-drift pre-clear consumed (drift_id=" + driftID + ")",
			}
			inputs.Precomputed = &allow
			return inputs
		}
		if out, ok := cache[tu.ID]; ok {
			if out.Err != nil {
				inputs.PrecomputedErr = out.Err
			} else {
				dec := out.Decision
				inputs.Precomputed = &dec
			}
		}
		return inputs
	}

	prefetch := func(ctx context.Context, toolUses []conversation.ToolUse) {
		// Gather the inputs EvaluateAuthorization would actually
		// consume for each sibling: only tool_uses the inspector
		// classifies as trigger-miss AND that have policy config or
		// sensitive-path. Tool_uses outside that set short-circuit in
		// AuthorizationPolicy.Evaluate without a decision-engine call,
		// so batching them would be wasted work.
		type pending struct {
			tuID  string
			input runtimedecision.AuthorizationInput
		}
		pendings := make([]pending, 0, len(toolUses))
		for _, tu := range toolUses {
			v := rewrite.Inspector.Inspect(ctx, inspector.ToolUse{
				ID:    tu.ID,
				Name:  tu.Name,
				Input: tu.Input,
			})
			if v.Source != inspector.SourceTriggerMiss {
				continue
			}
			inputs := planFor(tu)
			if !inputs.HasPolicyConfig && !inputs.ShellSensitivePath {
				continue
			}
			// Mirror AuthorizationPolicy.Evaluate's SkipIntentVerification
			// assignment so the cached decision matches the inline call.
			in := inputs.Input
			in.SkipIntentVerification = inputs.ReadOnlyShellCommand
			pendings = append(pendings, pending{tuID: tu.ID, input: in})
		}
		if len(pendings) == 0 {
			return
		}
		batchInputs := make([]runtimedecision.AuthorizationInput, len(pendings))
		for i, p := range pendings {
			batchInputs[i] = p.input
		}
		outcomes := runtimedecision.EvaluateAuthorizationBatch(ctx, batchInputs)
		for i, out := range outcomes {
			cache[pendings[i].tuID] = out
		}
	}

	return authorizationResolverBundle{Resolve: resolve, Prefetch: prefetch}
}

// detectShellSpecials derives read-only-shell and sensitive-path flags
// so the resolver hands AuthorizationPolicy the right inputs. Returns
// (readOnlyShellCommand, sensitivePath); when sensitivePath is true
// readOnlyShellCommand is forced false (sensitive overrides).
func detectShellSpecials(tu conversation.ToolUse, agent llmproxy.AgentContext, auth llmproxy.AuthorizationContext) (bool, bool) {
	if !toolnames.IsShellToolName(tu.Name) {
		return false, false
	}
	cmd := shellpolicy.ShellCommandFromInput(tu.Input)
	if cmd == "" {
		return false, false
	}
	if toolnames.SensitiveFileGuardEnabled(tu.Name, agent.AgentID, auth.ToolRules) {
		if _, _, hit := inspector.CommandReferencesSensitivePath(cmd); hit {
			return false, true
		}
	}
	if !shellpolicy.ReadOnlyShellCommandsAllowed(tu.Name, agent.AgentID, auth.ToolRules) {
		return false, false
	}
	readOnly, _ := inspector.IsReadOnlyBashCommand(cmd)
	return readOnly, false
}

func runIntentVerify(ctx context.Context, verifier llmproxy.IntentVerifier, dec llmproxy.TaskScopeDecision, resolved llmproxy.ResolvedAction, tu conversation.ToolUse) (string, bool) {
	purpose := ""
	if dec.MatchedTask != nil {
		purpose = dec.MatchedTask.Purpose
	}
	verification := ""
	expectedUse := ""
	hasAction := dec.MatchedAction != nil
	if hasAction {
		verification = dec.MatchedAction.Verification
		expectedUse = dec.MatchedAction.ExpectedUse
	}
	return intentverify.Run(ctx, verifier, intentverify.Decision{
		TaskID:       dec.TaskID,
		TaskPurpose:  purpose,
		ExpectedUse:  expectedUse,
		Verification: verification,
		HasAction:    hasAction,
	}, intentverify.ResolvedAction{
		ServiceID: resolved.ServiceID,
		ActionID:  resolved.ActionID,
	}, tu, func(err error) bool {
		return errors.Is(err, llmproxy.ErrCircuitOpen)
	})
}

// authorizationHoldHandler implements policies.AuthorizationHoldHandler
// for AuthorizationPolicy's approval flow. On a scope-drift source
// the coordinator mints a drift and the handler returns
// ContinueWithToolResult so the policy surfaces the agent-facing menu
// instead of parking a user hold. Otherwise commits the hold via
// PendingApprovals.Hold, renders the approval prompt, and cleans up
// any evicted inline task.
type authorizationHoldHandler struct {
	agent    llmproxy.AgentContext
	audit    llmproxy.AuditContext
	approval llmproxy.ApprovalContext
	provider conversation.Provider
	drifts   *scopeDriftCoordinator
}

func (h *authorizationHoldHandler) Hold(ctx context.Context, req policies.AuthorizationHoldRequest) (policies.AuthorizationHoldResult, error) {
	// Scope-drift continuation: when the block is task_scope_missing /
	// task_scope_ambiguous AND a registry is wired, the coordinator
	// mints a drift and returns the menu text. Layer 2 hardcoded
	// approvals (SourceRuleReview etc.) keep the existing user-prompt
	// path because AppliesToSource returns false for them. On mint
	// failure we fall through to the legacy approval-hold path rather
	// than emit a menu with no usable drift_id.
	if mint := h.drifts.MintForTriggerMiss(ctx, req.ToolUse, req.InspectorVerdict, req.Decision); mint.OK {
		return policies.AuthorizationHoldResult{
			ContinueWithToolResult: mint.MenuText,
			SubstituteText:         mint.MenuText,
		}, nil
	}
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
		SubstituteText: approvaltext.ApprovalPrompt(req.ToolUse, req.Decision.Reason, approvalID),
	}, nil
}

// buildScriptSessionResolver pins the resolver to the proxy's
// /api/proxy mount so the policy can recognize already-rewritten
// script-session curls, and threads the LLM judge through so the
// evaluator can re-classify URL-unrecognized tool_uses.
//
// RewriteContext supplies the resolver base URL (routing concern);
// ScriptSessionContext supplies the judge (recognition concern).
// Splitting them keeps the rewrite context focused on rewriting
// rather than becoming a kitchen-sink of cross-stage dependencies.
func buildScriptSessionResolver(rewrite llmproxy.RewriteContext, scriptSess llmproxy.ScriptSessionContext) policies.ScriptSessionResolver {
	if rewrite.RewriteOpts.ResolverBaseURL == "" {
		return nil
	}
	resolverBaseURL := rewrite.RewriteOpts.ResolverBaseURL
	judge := scriptSess.Judge
	return func(_ context.Context, _ conversation.ToolUse) *policies.ScriptSessionInputs {
		return &policies.ScriptSessionInputs{
			ResolverBaseURL: resolverBaseURL,
			Judge:           judge,
		}
	}
}

// buildRewriteResolver wires CredentialRewriteEvaluator to the
// inspector + nonce cache + rewrite opts that the rewrite stage
// needs.
//
// AgentContext supplies identity; RewriteContext supplies Inspector,
// CallerNonces, and RewriteOpts.
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

// isLegacyScopeDriftReason reports whether a TaskScope.Check denial
// reason represents an actual missing-scope situation (vs. a
// backend / config / programmer error). Only the actual-miss cases
// route through the scope-drift menu; the others hard-block so an
// operator sees them rather than letting an agent ride out a
// degraded backend by collecting a pre-clear.
//
// The reason strings are the ones StoreTaskScopeChecker.Check
// produces (see internal/runtime/llmproxy/taskscope.go) plus the
// post-classification cases from classifyToDecision. Whitelist
// the safe ones rather than blacklist the unsafe ones — new
// failure modes added to the checker default to hard-block,
// which is the right safety bias.
func isLegacyScopeDriftReason(reason string) bool {
	switch reason {
	case "needs_new_task", "no_active_task":
		return true
	}
	return false
}
