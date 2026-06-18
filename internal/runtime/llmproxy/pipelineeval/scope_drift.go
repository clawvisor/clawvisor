package pipelineeval

import (
	"context"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/inspector"
	runtimedecision "github.com/clawvisor/clawvisor/pkg/runtime/decision"
)

// scopeDriftCoordinator concentrates the scope-drift menu integration
// the factory used to scatter across applyModernDecision, the resolve
// closure, and authorizationHoldHandler.Hold. The coordinator is a
// pure helper: it owns the per-call decisions (is this a drift source,
// does a pre-clear apply, mint a drift), but the factory still owns
// the audit/verdict translation each call site needs.
//
// All methods are safe to call when ScopeDrifts is nil; they return
// the zero value, telling the caller to fall through to its legacy
// path. That nil-tolerance is the whole reason the wiring lives in a
// coordinator: handler boots that don't configure a registry
// (legacy tests, unconfigured deployments) keep behaving exactly the
// way they did before scope drift existed.
type scopeDriftCoordinator struct {
	agent          llmproxy.AgentContext
	audit          llmproxy.AuditContext
	registry       llmproxy.ScopeDriftRegistry
	catalog        interface {
		Resolve(host, method, path string) (llmproxy.ResolvedAction, bool)
	}
	provider       conversation.Provider
	controlBaseURL string
}

// newScopeDriftCoordinator extracts the sub-context references the
// coordinator needs. Returns a coordinator whose ScopeDrifts is nil
// when the caller didn't wire a registry — every method tolerates that
// shape and signals fall-through to the caller.
func newScopeDriftCoordinator(
	agent llmproxy.AgentContext,
	audit llmproxy.AuditContext,
	auth llmproxy.AuthorizationContext,
	routing llmproxy.RoutingContext,
	provider conversation.Provider,
) *scopeDriftCoordinator {
	return &scopeDriftCoordinator{
		agent:          agent,
		audit:          audit,
		registry:       auth.ScopeDrifts,
		catalog:        auth.Catalog,
		provider:       provider,
		controlBaseURL: routing.ControlBaseURL,
	}
}

// AppliesToSource reports whether a decision's source is one a drift
// mint covers: a missing/ambiguous task scope OR an intent-verifier
// refusal. All three options the menu offers — expand the task to
// cover this action, create a new task that covers it, or one-off
// — are reasonable recoveries from either flavor of denial: the
// scope-drift cases ("no task covers this action") and the
// intent-refusal cases ("the active task's purpose doesn't match
// what this call is doing"). Layer 2 hardcoded approval rules
// (SourceRuleReview etc.) keep their existing user-prompt path
// because they're not recoverable via task adjustments.
func (c *scopeDriftCoordinator) AppliesToSource(source runtimedecision.DecisionSource) bool {
	switch source {
	case runtimedecision.SourceTaskScopeMissing, runtimedecision.SourceTaskScopeAmbiguous, runtimedecision.SourceIntentRefusal:
		return true
	}
	return false
}

// driftSourceFor maps a runtimedecision source to the registry-level
// ScopeDriftSource recorded on the drift. Telemetry can distinguish
// task-scope misses from intent-verifier refusals via this field.
func driftSourceFor(source runtimedecision.DecisionSource) llmproxy.ScopeDriftSource {
	if source == runtimedecision.SourceIntentRefusal {
		return llmproxy.ScopeDriftSourceIntentVerification
	}
	return llmproxy.ScopeDriftSourceTaskScope
}

// MintResult is the coordinator's signal to its caller. MenuText is
// the rendered menu; the caller surfaces it to the agent by:
//   1. Rewriting the blocked tool_use into Sentinel (a canonical Bash
//      no-op encoding the original call) so the harness's local
//      execution is harmless.
//   2. Attaching Spec to the resulting verdict so the postprocess
//      layer registers the inbound substitution after the verdict is
//      finalized. The coordinator no longer writes to the registry
//      itself — that side-effecting step is owned by postprocess,
//      keeping the verdict pure data.
// driftID is the registered drift's id, for audit linkage. OK reports
// whether the mint actually landed — a false return tells the caller
// to fall through to its legacy approval-prompt path.
type MintResult struct {
	MenuText string
	DriftID  string
	Sentinel *conversation.SyntheticToolCall
	Spec     *conversation.PendingSubstitutionSpec
	Err      error
	OK       bool
}

// MintForCredentialed registers a drift for the credentialed
// (is_api_call=true) path. ResolvedAction is supplied by the caller
// because the credentialed path already resolved the catalog at plan
// time and we don't want to repeat that lookup here.
func (c *scopeDriftCoordinator) MintForCredentialed(
	ctx context.Context,
	tu conversation.ToolUse,
	v inspector.Verdict,
	resolved llmproxy.ResolvedAction,
	taskID string,
	dec runtimedecision.AuthorizationDecision,
) MintResult {
	if c == nil || c.registry == nil || !c.AppliesToSource(dec.Source) {
		return MintResult{}
	}
	template := llmproxy.ScopeDrift{
		UserID:         c.agent.AgentUserID,
		AgentID:        c.agent.AgentID,
		ConversationID: c.audit.ConversationID,
		Provider:       c.provider,
		ToolUse:        tu,
		Service:        resolved.ServiceID,
		Action:         resolved.ActionID,
		Host:           v.Host,
		Method:         v.Method,
		Path:           v.Path,
		TaskID:         taskID,
		Source:         driftSourceFor(dec.Source),
		ReasonText:     dec.Reason,
	}
	menuText, driftID, mintErr := llmproxy.BuildScopeDriftContinuation(ctx, c.registry, template, c.controlBaseURL)
	if mintErr != nil {
		return MintResult{Err: mintErr}
	}
	sentinel := buildScopeDriftSentinel(driftID, tu)
	return MintResult{
		MenuText: menuText,
		DriftID:  driftID,
		Sentinel: sentinel,
		Spec:     buildPendingSubstitutionSpec(tu, driftID, menuText),
		OK:       true,
	}
}

// MintForTriggerMiss registers a drift for the non-credentialed
// (trigger-miss) path. Unlike the credentialed path, the resolver
// hasn't pre-resolved a (service, action) — Bash/Edit/Read don't have
// one. The drift carries Host/Method/Path the inspector inferred (when
// available) so the fingerprint remains stable across the retry.
func (c *scopeDriftCoordinator) MintForTriggerMiss(
	ctx context.Context,
	tu conversation.ToolUse,
	v inspector.Verdict,
	dec runtimedecision.AuthorizationDecision,
) MintResult {
	if c == nil || c.registry == nil || !c.AppliesToSource(dec.Source) {
		return MintResult{}
	}
	template := llmproxy.ScopeDrift{
		UserID:         c.agent.AgentUserID,
		AgentID:        c.agent.AgentID,
		ConversationID: c.audit.ConversationID,
		Provider:       c.provider,
		ToolUse:        tu,
		Host:           v.Host,
		Method:         v.Method,
		Path:           v.Path,
		Source:         driftSourceFor(dec.Source),
		ReasonText:     dec.Reason,
	}
	menuText, driftID, mintErr := llmproxy.BuildScopeDriftContinuation(ctx, c.registry, template, c.controlBaseURL)
	if mintErr != nil {
		return MintResult{Err: mintErr}
	}
	sentinel := buildScopeDriftSentinel(driftID, tu)
	return MintResult{
		MenuText: menuText,
		DriftID:  driftID,
		Sentinel: sentinel,
		Spec:     buildPendingSubstitutionSpec(tu, driftID, menuText),
		OK:       true,
	}
}

// buildScopeDriftSentinel constructs the SyntheticToolCall that
// replaces the blocked tool_use in the response rewriter. The
// tool_use_id is preserved so the inbound rewriter can later restore
// the original call and substitute the matching tool_result's content.
//
// The placeholder is a harness-side artifact only — the LLM never sees
// it. On the next /v1/messages the inbound rewriter walks back the
// substitution: original tool_use restored byte-for-byte, tool_result
// content replaced with the menu.
func buildScopeDriftSentinel(driftID string, tu conversation.ToolUse) *conversation.SyntheticToolCall {
	command := llmproxy.BuildScopeDriftPlaceholderCommand(tu.Name, driftID)
	return &conversation.SyntheticToolCall{
		ID:   tu.ID,
		Name: llmproxy.ScopeDriftPlaceholderToolName,
		Input: map[string]any{
			"command": command,
		},
	}
}

// buildPendingSubstitutionSpec collects every field the inbound
// rewriter needs to restore the original tool_use and replace the
// tool_result content on the next /v1/messages. The spec is attached
// to the verdict and registered later by postprocess — the coordinator
// itself stays free of registry side-effects so the verdict remains
// pure data.
func buildPendingSubstitutionSpec(tu conversation.ToolUse, driftID, menuText string) *conversation.PendingSubstitutionSpec {
	return &conversation.PendingSubstitutionSpec{
		DriftID:           driftID,
		MenuText:          menuText,
		OriginalToolName:  tu.Name,
		OriginalToolInput: append([]byte(nil), tu.Input...),
	}
}

// ConsumePreClear looks up a one-shot pre-clear for the agent's retry
// of a CREDENTIALED originally-blocked tool_use. The lookup is keyed
// on the fingerprint of (agent, conversation, service, action, host,
// method, path, input bytes) — see ScopeDrift.Fingerprint. A hit
// CONSUMES the entry; a sibling call that doesn't match has to mint a
// fresh drift.
//
// Returns (driftID, true) on hit; ("", false) when the registry is
// unwired, the catalog can't resolve the call's (service, action), or
// no pre-clear exists. Use ConsumePreClearForTriggerMiss for the
// non-credentialed (Bash/Edit/Read) path — that path has no resolved
// (service, action) at retry time and would always miss this lookup.
func (c *scopeDriftCoordinator) ConsumePreClear(
	ctx context.Context,
	tu conversation.ToolUse,
	v inspector.Verdict,
) (string, bool) {
	if c == nil || c.registry == nil || c.catalog == nil {
		return "", false
	}
	resolved, ok := c.catalog.Resolve(v.Host, v.Method, v.Path)
	if !ok || resolved.ServiceID == "" || resolved.ActionID == "" {
		return "", false
	}
	fp := llmproxy.ScopeDrift{
		AgentID:        c.agent.AgentID,
		ConversationID: c.audit.ConversationID,
		ToolUse:        tu,
		Service:        resolved.ServiceID,
		Action:         resolved.ActionID,
		Host:           v.Host,
		Method:         v.Method,
		Path:           v.Path,
	}.Fingerprint()
	return c.registry.LookupPreClear(ctx, c.agent.AgentID, fp)
}

// ConsumePreClearForTriggerMiss looks up a one-shot pre-clear for the
// non-credentialed retry path (Bash/Edit/Read). The fingerprint shape
// matches MintForTriggerMiss exactly: no Service/Action (the inspector
// never assigned any), just (agent, conversation, host, method, path,
// input bytes).
//
// Without this lookup the trigger-miss approval lifecycle leaks: an
// approved one-off mints a Succeeded pre-clear under the trigger-miss
// fingerprint, but the next call hits AuthorizationPolicy →
// VerdictNeedsApproval → mints a FRESH drift, and the agent loops.
// Expand and new_task approvals on this path also benefit when the
// rebuilt task scope doesn't yet cover the call shape — though the
// usual recovery is via EvaluateAuthorization seeing the new task.
func (c *scopeDriftCoordinator) ConsumePreClearForTriggerMiss(
	ctx context.Context,
	tu conversation.ToolUse,
	v inspector.Verdict,
) (string, bool) {
	if c == nil || c.registry == nil {
		return "", false
	}
	fp := llmproxy.ScopeDrift{
		AgentID:        c.agent.AgentID,
		ConversationID: c.audit.ConversationID,
		ToolUse:        tu,
		Host:           v.Host,
		Method:         v.Method,
		Path:           v.Path,
	}.Fingerprint()
	return c.registry.LookupPreClear(ctx, c.agent.AgentID, fp)
}
