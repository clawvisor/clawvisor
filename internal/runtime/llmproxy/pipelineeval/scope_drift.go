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
// mint covers: task scope missing or ambiguous. Layer 2 hardcoded
// approval rules (SourceRuleReview etc.) and other sources are NOT
// scope drift and keep their existing user-prompt path.
func (c *scopeDriftCoordinator) AppliesToSource(source runtimedecision.DecisionSource) bool {
	switch source {
	case runtimedecision.SourceTaskScopeMissing, runtimedecision.SourceTaskScopeAmbiguous:
		return true
	}
	return false
}

// MintResult is the coordinator's signal to its caller. menuText is
// the rendered menu the caller should propagate via Continue +
// SubstituteText (so the continuation round-trip carries the menu and
// the harness fallback shows it on a continuation miss). driftID is
// the registered drift's id, for audit linkage. OK reports whether
// the mint actually landed — a false return tells the caller to fall
// through to its legacy approval-prompt path.
type MintResult struct {
	MenuText string
	DriftID  string
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
		Source:         llmproxy.ScopeDriftSourceTaskScope,
		ReasonText:     dec.Reason,
	}
	menuText, driftID, mintErr := llmproxy.BuildScopeDriftContinuation(ctx, c.registry, template, c.controlBaseURL)
	if mintErr != nil {
		return MintResult{Err: mintErr}
	}
	return MintResult{MenuText: menuText, DriftID: driftID, OK: true}
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
		Source:         llmproxy.ScopeDriftSourceTaskScope,
		ReasonText:     dec.Reason,
	}
	menuText, driftID, mintErr := llmproxy.BuildScopeDriftContinuation(ctx, c.registry, template, c.controlBaseURL)
	if mintErr != nil {
		return MintResult{Err: mintErr}
	}
	return MintResult{MenuText: menuText, DriftID: driftID, OK: true}
}

// ConsumePreClear looks up a one-shot pre-clear for the agent's retry
// of an originally-blocked tool_use. The lookup is keyed on the
// fingerprint of (agent, conversation, service, action, host, method,
// path, input bytes) — see ScopeDrift.Fingerprint. A hit CONSUMES the
// entry; a sibling call that doesn't match has to mint a fresh drift.
//
// Returns (driftID, true) on hit; ("", false) when the registry is
// unwired, the catalog can't resolve the call's (service, action), or
// no pre-clear exists.
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
