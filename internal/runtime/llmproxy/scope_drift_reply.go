package llmproxy

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// ScopeDriftReplyRewriteRequest is the input to
// RewriteScopeDriftOneOffApprovalReply. Mirrors the
// InlineApprovalRewriteRequest shape so callers can wire the two
// rewriters back-to-back from the same llm endpoint handler.
type ScopeDriftReplyRewriteRequest struct {
	HTTPRequest     *http.Request
	Provider        conversation.Provider
	Body            []byte
	Agent           *store.Agent
	ConversationID  string
	PendingApproval PendingApprovalCache
	ScopeDrifts     ScopeDriftRegistry
	Logger          *slog.Logger
}

// ScopeDriftReplyRewriteResult is what the handler does with the
// rewritten request after a scope-drift one-off approval reply.
type ScopeDriftReplyRewriteResult struct {
	Body      []byte
	Rewritten bool
	Decision  string // "allow" | "deny" | ""
	Outcome   string // outcome tag for audit
	Reason    string // human-readable detail, included in audit
	DriftID   string // populated on a successful match for audit linkage
}

// RewriteScopeDriftOneOffApprovalReply handles the user's "yes"/"no"
// reply to a one-off approval prompt the resolver queued under
// StageAwaitingScopeDriftOneOff. The proxy flips the drift outcome
// (Succeeded on approve, Denied on deny) and rewrites the user's
// message so the model sees a synthesized Clawvisor status line
// instead of the raw "yes"/"no" — without the rewrite the model would
// be confused by a bare approval reply that lacks any reference to
// what it was approving.
//
// When the most recent hold isn't a scope-drift one-off (or there is
// no hold, or the reply verb isn't approve/deny), this returns
// (req.Body, Rewritten=false, nil) and the caller proceeds to the
// next rewriter (typically RewriteInlineTaskApprovalReply).
func RewriteScopeDriftOneOffApprovalReply(ctx context.Context, req ScopeDriftReplyRewriteRequest) (ScopeDriftReplyRewriteResult, error) {
	out := ScopeDriftReplyRewriteResult{Body: req.Body}
	if req.PendingApproval == nil || req.Agent == nil || req.ScopeDrifts == nil {
		return out, nil
	}
	editor, ok := newApprovalBodyEditor(req.HTTPRequest, req.Provider, req.Body)
	if !ok {
		return out, nil
	}
	verb, approvalID, ok := editor.LatestApprovalReply()
	if !ok || (verb != "approve" && verb != "deny") {
		return out, nil
	}

	action, err := resolveApprovalReplyAction(ctx, approvalReplyRoutingRequest{
		UserID:          req.Agent.UserID,
		AgentID:         req.Agent.ID,
		Provider:        req.Provider,
		ConversationID:  req.ConversationID,
		PendingApproval: req.PendingApproval,
		Verb:            verb,
		ApprovalID:      approvalID,
	})
	if err != nil {
		return out, err
	}
	if action.Kind != approvalReplyActionApproveScopeDriftOneOff && action.Kind != approvalReplyActionDenyScopeDriftOneOff {
		return out, nil
	}
	if action.Hold == nil {
		return out, nil
	}

	// Probe the body editor BEFORE consuming the hold, mirroring the
	// inline-task rewriter's pattern: if the body shape is one we
	// can't rewrite, fail closed without disturbing cache state so a
	// fixed retry can drive the flow.
	expectedApprovalID := action.Hold.ID
	_, canRewrite, probeErr := editor.ReplaceLatestUserText(verb, expectedApprovalID, "")
	if probeErr != nil {
		return out, probeErr
	}
	if !canRewrite {
		out.Decision = "deny"
		out.Outcome = "scope_drift_body_rewrite_unsupported"
		out.Reason = "could not rewrite user message in current request body shape"
		return out, nil
	}

	// Consume the hold. Resolve by explicit ID so a concurrent Hold
	// landing between Peek and Resolve can't surface a different
	// newest hold.
	resolved, err := req.PendingApproval.Resolve(ctx, ResolveRequest{
		UserID:         req.Agent.UserID,
		AgentID:        req.Agent.ID,
		Provider:       req.Provider,
		ConversationID: req.ConversationID,
		ApprovalID:     action.Hold.ID,
	})
	if err != nil {
		return out, err
	}
	if resolved == nil {
		return out, nil
	}
	out.DriftID = resolved.ScopeDriftID

	logger := req.Logger
	if logger == nil {
		logger = slog.Default()
	}

	var replacement string
	if verb == "approve" {
		// SetOutcome(Succeeded) inserts the one-shot pre-clear keyed
		// by (agent, fingerprint). The agent's next attempt of the
		// original blocked tool_use will consume it and pass scope+
		// intent verification.
		if err := req.ScopeDrifts.SetOutcome(ctx, resolved.ScopeDriftID, ScopeDriftOutcomeSucceeded); err != nil {
			// The hold is already consumed and we can't insert the
			// pre-clear. Surface a denial so the model gets honest
			// feedback rather than thinking the call is cleared
			// when it isn't.
			//
			// Best-effort flip to Denied so the drift_id isn't left
			// stranded at ChosenOption=one_off, Outcome=pending —
			// without this, future status polls would report the
			// drift as still in-flight even though the user has
			// already replied, and the agent could be misled into
			// thinking another approval is coming. If the Denied
			// write also fails, the drift will TTL out (~60s) so
			// the dead-end window is bounded.
			logger.ErrorContext(ctx, "scope-drift one-off approval pre-clear write failed",
				"drift_id", resolved.ScopeDriftID, "err", err)
			if denyErr := req.ScopeDrifts.SetOutcome(ctx, resolved.ScopeDriftID, ScopeDriftOutcomeDenied); denyErr != nil {
				logger.WarnContext(ctx, "scope-drift one-off post-failure denied write also failed; drift will TTL out",
					"drift_id", resolved.ScopeDriftID, "err", denyErr)
			}
			out.Decision = "deny"
			out.Outcome = "scope_drift_pre_clear_failed"
			out.Reason = err.Error()
			replacement = "[Clawvisor scope-drift] Pre-clear write failed (" + sanitizeStatusValue(err.Error()) + "). The drift is closed; re-emit the original tool call to start over with a fresh drift_id."
		} else {
			out.Decision = "allow"
			out.Outcome = "scope_drift_one_off_approved"
			replacement = "[Clawvisor scope-drift] Your one-off approval landed for drift " + resolved.ScopeDriftID + ". Re-emit the original tool call unchanged — Clawvisor pre-clears it once on this drift_id."
		}
	} else {
		if err := req.ScopeDrifts.SetOutcome(ctx, resolved.ScopeDriftID, ScopeDriftOutcomeDenied); err != nil {
			logger.WarnContext(ctx, "scope-drift one-off deny outcome write failed",
				"drift_id", resolved.ScopeDriftID, "err", err)
		}
		out.Decision = "deny"
		out.Outcome = "scope_drift_one_off_denied"
		replacement = "[Clawvisor scope-drift] The one-off was denied. This drift_id is now closed. Do not retry under it — re-emit the original tool call only after you have a new plan (a fresh expand, a new task, or a different approach)."
	}

	rewritten, ok, err := editor.ReplaceLatestUserText(verb, resolved.ID, replacement)
	if err != nil {
		return out, err
	}
	if !ok {
		return out, nil
	}
	out.Body = rewritten
	out.Rewritten = true
	return out, nil
}
