package policies

import (
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/inspector"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/pipeline"
)

// ToolUseChainConfig bundles the resolvers + dependencies needed to
// assemble the six-stage tool_use evaluator chain that replaces
// newToolUseEvaluator in postprocess.go.
//
// All fields are optional in the sense that nil resolvers degrade
// gracefully (the corresponding evaluator emits Skip). The handler
// supplies non-nil resolvers for the capabilities it actually wants
// active for the current call:
//
//	chain := policies.ComposeToolUseEvaluatorChain(policies.ToolUseChainConfig{
//	    Control:         buildControlResolver(h),
//	    ScriptSession:   buildScriptSessionResolver(h),
//	    Inspector:       h.Inspector,
//	    AllowedHostsFor: buildAllowedHostsResolver(h),
//	    TriggerMissAuth: buildTriggerMissAuthorizer(h),
//	    TaskScope:       buildCredentialedTaskScopeResolver(h),
//	    IntentVerify:    buildIntentVerifyResolver(h),
//	    Rewrite:         buildCredentialRewriteResolver(h),
//	})
//	eval, _, err := pipeline.BridgeToolUseEvaluator(ctx, res, toolUses, chain)
type ToolUseChainConfig struct {
	// Control claims tool_uses that target the control plane (clawvisor.local).
	Control ControlToolUseResolver
	// ScriptSession claims tool_uses pre-shaped for the resolver mount.
	ScriptSession ScriptSessionResolver
	// Inspector + AllowedHostsFor + TriggerMissAuth drive the
	// inspect → ambiguous → boundary-check → trigger-miss-authorization
	// chain stage.
	Inspector       *inspector.Inspector
	AllowedHostsFor AllowedHostsResolver
	// Boundary, when set, overrides AllowedHostsFor with a typed
	// resolver that emits discrete denial reasons. Prefer this in new
	// callers; AllowedHostsFor is retained for backward compat and
	// adapted via boundaryResolverFromHosts when Boundary is nil.
	Boundary        BoundaryResolver
	TriggerMissAuth TriggerMissAuthorizer
	// ReadOnlyShell, when set, wires ReadOnlyShellPassthroughPolicy +
	// SensitivePathPolicy. They share the same per-call inputs
	// (AgentID + ToolRules) so they share one resolver. Nil → both
	// policies Skip.
	ReadOnlyShell ReadOnlyShellResolver
	// Authorization, when set, wires AuthorizationPolicy. The resolver
	// produces AuthorizationInputs (decision-engine inputs + hold
	// handler) per-call. AuthorizationPolicy replaces the legacy
	// EvaluateTriggerMissAuthorization closure path for trigger-miss
	// tool_uses. Nil → policy Skips.
	Authorization AuthorizationResolver
	// TaskScope authorizes credentialed tool_uses against the agent's
	// active task scopes (the handler wraps EvaluateAuthorization +
	// catalog resolution into the TaskScopeResolver closure).
	TaskScope TaskScopeResolver
	// IntentVerify runs the LLM-backed intent check for matched tasks.
	IntentVerify IntentVerifyResolver
	// Rewrite mints the per-tool nonce and rewrites the tool_use's URL
	// + caller-token header so the call routes through the resolver.
	Rewrite CredentialRewriteResolver
}

// ComposeToolUseEvaluatorChain assembles the six-stage tool_use
// evaluator chain in the order the legacy newToolUseEvaluator runs them:
//
//  1. ControlToolUseEvaluator — claims control-plane tool_uses (with
//     inline-task interception when configured).
//  2. ScriptSessionEvaluator — claims tool_uses already shaped for the
//     resolver mount via a script-session token.
//  3. InspectorChain — inspect + ambiguous + stub-placeholder downgrade
//     + boundary check + trigger-miss authorization.
//  4. TaskScopeEvaluator — credentialed-path task-scope authorization.
//  5. IntentVerifyEvaluator — LLM-backed intent check for matched tasks.
//  6. CredentialRewriteEvaluator — nonce mint + URL rewrite.
//
// Earlier evaluators short-circuit later ones via the orchestrator's
// "first non-Skip wins" semantic. Nil resolvers degrade to Skip so the
// chain works in partial configurations (e.g., a deployment without
// task scopes still gets the inspector + rewrite path).
func ComposeToolUseEvaluatorChain(cfg ToolUseChainConfig) []pipeline.ToolUseEvaluator {
	inspectorChain := NewInspectorChain(cfg.Inspector, cfg.AllowedHostsFor)
	if cfg.Boundary != nil {
		inspectorChain = inspectorChain.WithBoundaryResolver(cfg.Boundary)
	}
	inspectorChain = inspectorChain.WithTriggerMissAuthorizer(cfg.TriggerMissAuth)
	chain := make([]pipeline.ToolUseEvaluator, 0, 8)
	chain = append(chain, NewControlToolUseEvaluator(cfg.Control))
	chain = append(chain, NewScriptSessionEvaluator(cfg.ScriptSession))
	// Phase 6 decomposed policies run BEFORE InspectorChain so a
	// background-shell poll claims Allow directly (the legacy
	// EvaluateTriggerMissAuthorization closure would have done the same
	// via InspectorChain's TriggerMissAuth). The other Phase 6 policies
	// (ReadOnlyShell, SensitivePath, Authorization) are built but not
	// wired yet — they require resolver plumbing the host hasn't moved
	// out of the closure pattern. ShellPoll has no host dependencies, so
	// it lands first.
	// Phase 6 decomposed policies handling trigger-miss specials. Run
	// BEFORE InspectorChain so they claim Allow/Deny/Hold directly
	// without delegating to the legacy TriggerMissAuthorizer closure.
	// SensitivePath emits a Fact-only Skip; AuthorizationPolicy reads
	// the sensitive trail + runs EvaluateAuthorization + handles the
	// approval flow inline.
	chain = append(chain, NewSensitivePathPolicy(cfg.Inspector, cfg.ReadOnlyShell))
	chain = append(chain, NewShellPollPassthroughPolicy(cfg.Inspector))
	chain = append(chain, NewReadOnlyShellPassthroughPolicy(cfg.Inspector, cfg.ReadOnlyShell))
	chain = append(chain, NewAuthorizationPolicy(cfg.Inspector, cfg.Authorization))
	chain = append(chain, inspectorChain)
	chain = append(chain, NewTaskScopeEvaluator(cfg.TaskScope))
	chain = append(chain, NewIntentVerifyEvaluator(cfg.IntentVerify))
	chain = append(chain, NewCredentialRewriteEvaluator(cfg.Rewrite))
	return chain
}
