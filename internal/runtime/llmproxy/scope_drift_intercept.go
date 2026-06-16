package llmproxy

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
)

// Path shape: /api/control/scope-drifts/<drift_id>/one-off.
const (
	scopeDriftOneOffPathPrefix = "/api/control/scope-drifts/"
	scopeDriftOneOffPathSuffix = "/one-off"
)

// inlineScopeDriftOneOffBody is the shape the model posts to claim
// option (c). `rationale` is shown verbatim to the user in the
// approval prompt.
type inlineScopeDriftOneOffBody struct {
	Rationale string `json:"rationale"`
}

// MaybeInterceptScopeDriftOneOff routes a model-emitted
// POST /api/control/scope-drifts/<drift_id>/one-off?surface=inline
// tool_use through the scope-drift one-off approval flow. Mirror of
// MaybeInterceptInlineExpansion / MaybeInterceptInlineTaskDefinition:
// when the query signal fires, the model never actually POSTs — the
// tool_result is replaced with a rendered yes/no prompt and the
// user's "yes" lands SetOutcome(Succeeded) on the drift so the
// agent's retry of the originally-blocked tool_use consumes the
// pre-clear.
//
// Returns (_, false) when the signal is absent, the body fails to
// parse, the drift_id is unknown, or the registry refuses the claim.
// Falling through to false lets the regular control-rewrite path
// handle the call (which produces a clean "unknown control endpoint"
// refusal the model can recover from on its own).
//
// Using a real POST endpoint instead of inline <clawvisor:decision>
// markup keeps option (c) structurally identical to (a) expand and
// (b) new_task: all three are normal control-plane POSTs the
// inspector + intercept chain already understands. The previous
// markup-based encoding required regex-parsing the assistant's free
// text and special-casing code fences to avoid self-matching on the
// menu's own example block — see git history for the bug class that
// removed.
func MaybeInterceptScopeDriftOneOff(
	req *http.Request,
	cfg PostprocessConfig,
	audit func(decision, outcome, reason string),
	trace func(event string, kv ...any),
	provider conversation.Provider,
	tu conversation.ToolUse,
	call ControlCall,
) (conversation.ToolUseVerdict, bool) {
	if cfg.PendingApprovals == nil || cfg.ScopeDrifts == nil {
		return conversation.ToolUseVerdict{}, false
	}
	if !strings.EqualFold(call.Method, "POST") {
		return conversation.ToolUseVerdict{}, false
	}
	path := call.URL.Path
	if !strings.HasPrefix(path, scopeDriftOneOffPathPrefix) || !strings.HasSuffix(path, scopeDriftOneOffPathSuffix) {
		return conversation.ToolUseVerdict{}, false
	}
	// Extract the {drift_id} segment. Mirror the expand intercept's
	// shape: middle must be a single non-empty segment with no further
	// slashes so attacker paths like /scope-drifts/x/y/one-off fail.
	mid := strings.TrimSuffix(strings.TrimPrefix(path, scopeDriftOneOffPathPrefix), scopeDriftOneOffPathSuffix)
	if mid == "" || strings.Contains(mid, "/") {
		return conversation.ToolUseVerdict{}, false
	}
	driftID := mid

	// Opt-in signal: same `?surface=inline` as task creation / expand.
	// A headless agent calling without the flag falls through to the
	// regular control-rewrite path and gets a "not found"-style
	// refusal it can recover from.
	querySignal := call.URL.Query().Get(InlineSurfaceQueryParam) == InlineSurfaceQueryValue
	if !querySignal {
		return conversation.ToolUseVerdict{}, false
	}

	bodyBytes, ok := controlTaskBodyFromInput(tu.Input)
	if !ok || len(bodyBytes) == 0 {
		audit("fallthrough", "inline_scope_drift_body_missing", "POST .../one-off had no body; deferring to dashboard rewrite")
		return conversation.ToolUseVerdict{}, false
	}
	parsed := inlineScopeDriftOneOffBody{}
	if err := json.Unmarshal(bodyBytes, &parsed); err != nil {
		audit("fallthrough", "inline_scope_drift_body_malformed", err.Error())
		return conversation.ToolUseVerdict{}, false
	}
	rationale := strings.TrimSpace(parsed.Rationale)
	if rationale == "" {
		audit("fallthrough", "inline_scope_drift_missing_rationale", "one-off body missing rationale; deferring to dashboard rewrite")
		return conversation.ToolUseVerdict{}, false
	}

	// Cap rationale length for the same reasons expand caps its
	// `reason` field: the value lands in approval prompts (Telegram
	// body, push action_summary), the canonical approval record, and
	// the audit row reason. 512 bytes is enough for any human-readable
	// one-liner.
	const maxRationaleLen = 512
	if len(rationale) > maxRationaleLen {
		audit("fallthrough", "inline_scope_drift_rationale_too_long", "rationale exceeds maximum length; deferring to dashboard rewrite")
		return conversation.ToolUseVerdict{}, false
	}

	ctx := req.Context()

	// Cross-agent / cross-conversation guard BEFORE the claim: a drift
	// minted for a different agent or conversation must not even be
	// readable here. Rejecting at peek time prevents a copied or
	// replayed drift_id from a different session from terminally
	// closing the original drift — calling SetOutcome(Denied) after
	// the claim succeeded would create a denial-of-service path where
	// anyone who learns a drift_id can permanently close someone
	// else's pending one-off. The legitimate session still owns the
	// drift after this refusal.
	existing, getErr := cfg.ScopeDrifts.Get(ctx, driftID)
	if errors.Is(getErr, ErrDriftNotFound) {
		audit("fallthrough", "inline_scope_drift_not_found", "drift "+driftID+" not found (it may have expired)")
		return conversation.ToolUseVerdict{}, false
	}
	if getErr != nil {
		audit("fallthrough", "inline_scope_drift_lookup_failed", getErr.Error())
		return conversation.ToolUseVerdict{}, false
	}
	if existing.AgentID != cfg.AgentID || existing.ConversationID != cfg.ConversationID {
		audit("fallthrough", "inline_scope_drift_wrong_agent_or_conversation", "drift "+driftID+" was minted for a different agent or conversation")
		return conversation.ToolUseVerdict{}, false
	}

	claimed, err := cfg.ScopeDrifts.ClaimOption(ctx, driftID, ScopeDriftOptionOneOff, rationale)
	if errors.Is(err, ErrDriftNotFound) {
		// Race: drift expired between the peek and the claim. Treat
		// as not-found rather than crashing the request.
		audit("fallthrough", "inline_scope_drift_not_found", "drift "+driftID+" not found (it may have expired)")
		return conversation.ToolUseVerdict{}, false
	}
	if errors.Is(err, ErrDriftAlreadyResolved) {
		audit("fallthrough", "inline_scope_drift_already_resolved", "drift "+driftID+" was already resolved with option "+string(claimed.ChosenOption))
		return conversation.ToolUseVerdict{}, false
	}
	if err != nil {
		audit("fallthrough", "inline_scope_drift_claim_failed", err.Error())
		return conversation.ToolUseVerdict{}, false
	}

	now := time.Now().UTC()
	hold, holdErr := cfg.PendingApprovals.Hold(ctx, PendingLiteApproval{
		UserID:              cfg.AgentUserID,
		AgentID:             cfg.AgentID,
		Provider:            provider,
		ConversationID:      cfg.ConversationID,
		ToolUse:             claimed.ToolUse,
		Reason:              "scope-drift one-off: " + claimed.Service + "." + claimed.Action,
		Stage:               StageAwaitingScopeDriftOneOff,
		ScopeDriftID:        claimed.ID,
		ScopeDriftAgentNote: rationale,
		CreatedAt:           now,
		ExpiresAt:           now.Add(inlineTaskApprovalHoldTTL),
	})
	if holdErr != nil {
		// Cache hold failed — close the drift so it isn't stranded
		// pending until TTL.
		_ = cfg.ScopeDrifts.SetOutcome(ctx, claimed.ID, ScopeDriftOutcomeDenied)
		audit("fallthrough", "inline_scope_drift_hold_failed", holdErr.Error()+"; deferring to dashboard rewrite")
		return conversation.ToolUseVerdict{}, false
	}

	audit("approve", "pending", "inline_scope_drift_pending_approval: awaiting user yes/no on one-off")
	trace("inline_scope_drift.held",
		"approval_id", hold.Pending.ID,
		"drift_id", claimed.ID,
		"signal", "query",
	)
	return conversation.ToolUseVerdict{
		Allowed:        false,
		Reason:         "Clawvisor: one-off approval pending — " + claimed.Service + "." + claimed.Action,
		SubstituteWith: renderScopeDriftOneOffPrompt(claimed, hold.Pending.ID),
	}, true
}
