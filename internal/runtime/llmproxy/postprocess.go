package llmproxy

import (
	"context"
	"errors"
	"strconv"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/approvaltext"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/inspector"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/intentverify"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/rewritehelp"
	runtimedecision "github.com/clawvisor/clawvisor/pkg/runtime/decision"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// IntentVerifier matches the intent.Verifier contract. The lite-proxy
// declares its own narrow interface to avoid pulling the LLM provider
// dependency into this package.

// CredentialedRewriteRecoveryReason is the user-facing recovery
// message for credential-rewrite errors. Used by the
// policies.CredentialRewriteEvaluator on rewriter_error.
func CredentialedRewriteRecoveryReason(v inspector.Verdict, err error) string {
	return rewritehelp.CredentialedRewriteRecoveryReason(v, err)
}

// coalesceFromCaptures builds the single PendingLiteApproval covering
// every tool_use in a turn. The first approval-needing capture becomes
// the primary (its decision context is mapped to the singular
// ToolUse/Inspector/Fingerprint/Reason fields the rest of the codebase
// already understands). PrimaryIndex records where the primary sat in
// the original turn, so AllHolds() — and the release path that emits
// from it — keep the model's tool_use order intact. Reordering would
// break dependent sequences like Bash producing stdout that a
// following WebFetch consumes.

// ApprovalPrompt renders the agent-facing message that substitutes for a
// paused tool call. When approvalID is non-empty, the InlineApprovalIDMarker
// footer is appended so subsequent turns can disambiguate which hold a bare
// "y"/"n" reply targets — important when one agent's transcript contains
// multiple pending prompts, or when several agents share a Clawvisor token
// and only the per-transcript marker reliably identifies the right hold.
func ApprovalPrompt(tu conversation.ToolUse, reason, approvalID string) string {
	return approvaltext.ApprovalPrompt(tu, reason, approvalID)
}

// DecisionIntentVerifierFor wraps a (possibly nil) IntentVerifier so
// runtimedecision.AuthorizationInput can consume it directly. The
// wrapper translates between the package-local IntentVerifyRequest /
// IntentVerdict types and runtimedecision's.
func DecisionIntentVerifierFor(v IntentVerifier) runtimedecision.IntentVerifier {
	return intentverify.DecisionVerifierFor(v)
}

// AuditAgentForCfg builds a minimal *store.Agent for the audit emitter
// from the postprocess config. The emitter only reads UserID and ID; we
// avoid an extra DB lookup by synthesizing the struct.
func AuditAgentForCfg(cfg PostprocessConfig) *store.Agent {
	if cfg.Audit == nil || cfg.AgentID == "" || cfg.AgentUserID == "" {
		return nil
	}
	return &store.Agent{ID: cfg.AgentID, UserID: cfg.AgentUserID}
}

// taskIDFromDecision extracts the matched task's ID from a decision,
// returning "" when there is no associated task. Trace-only helper.
func taskIDFromDecision(dec runtimedecision.AuthorizationDecision) string {
	if dec.Task == nil {
		return ""
	}
	return dec.Task.ID
}

// redactPlaceholderForReason returns the placeholder's prefix +
// length suffix — enough for operators to identify which placeholder
// was missing vs. which actually exists in the DB, without exposing
// the full random suffix in audit reasons that may surface in UIs or
// logs shared more broadly than the placeholder itself.
func redactPlaceholderForReason(ph string) string {
	const head = 18 // long enough to keep `autovault_<svc>_…`
	if len(ph) <= head {
		return ph
	}
	return ph[:head] + "…(" + strconv.Itoa(len(ph)) + " chars)"
}

// runIntentVerify runs LLM intent verification when the matched TaskAction
// opts in. Returns (reason, ok). ok=false on a refusal verdict; ok=true when
// the verifier was not consulted (off mode / missing dep) or returned Allow.
//
// Verification mode mapping (matches gateway behavior):
//   - "off"             → skip verification, allow.
//   - "lenient"         → call verifier with Lenient=true.
//   - "strict" / empty  → call verifier with Lenient=false.
//
// On verifier error we fail-open (audit will record), matching the gateway's
// behavior so a transient LLM outage doesn't block tool use; #37 will tighten
// this to fail-closed once the circuit breaker is in place.
// RunIntentVerify is the exported version of the per-task-scope intent
// check the credentialed path runs after TaskScope.Check confirms the
// scope match.
func RunIntentVerify(ctx context.Context, cfg PostprocessConfig, dec TaskScopeDecision, resolved ResolvedAction, tu conversation.ToolUse) (string, bool) {
	return runIntentVerify(ctx, cfg, dec, resolved, tu)
}

func runIntentVerify(ctx context.Context, cfg PostprocessConfig, dec TaskScopeDecision, resolved ResolvedAction, tu conversation.ToolUse) (string, bool) {
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
	return intentverify.Run(ctx, cfg.IntentVerifier, intentverify.Decision{
		TaskID:       dec.TaskID,
		TaskPurpose:  purpose,
		ExpectedUse:  expectedUse,
		Verification: verification,
		HasAction:    hasAction,
		OrgID:        cfg.AgentOrgID,
	}, intentverify.ResolvedAction{
		ServiceID: resolved.ServiceID,
		ActionID:  resolved.ActionID,
	}, tu, func(err error) bool {
		return errors.Is(err, ErrCircuitOpen)
	})
}

// matchByRoute resolves the response rewriter that pairs with the inbound
// route. The conversation.ResponseRegistry's MatchesResponse depends on
// the request's host (for runtime-proxy CONNECT use); for lite-proxy we
// dispatch by route path instead.
