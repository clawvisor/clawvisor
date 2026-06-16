package llmproxy

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// ScopeDriftReplyRewriteRequest is the input to
// RewriteScopeDriftOneOffApprovalReply.
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

// RewriteScopeDriftOneOffApprovalReply handles the user's yes/no reply
// to a one-off approval prompt the resolver queued under
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

	// Probe the body editor BEFORE consuming the hold: if the body
	// shape is one we can't rewrite, fail closed without disturbing
	// cache state so a fixed retry can drive the flow.
	expectedApprovalID := action.Hold.ID
	_, canRewrite, probeErr := editor.ReplaceLatestUserText(verb, expectedApprovalID, "", nil)
	if probeErr != nil {
		return out, probeErr
	}
	if !canRewrite {
		// The drift in the registry would otherwise sit at
		// ChosenOption=one_off / Outcome=pending until TTL, masking
		// a real failure as "still waiting for the user." Flip it
		// to Denied so a status poll surfaces the dead-end. Drop
		// the pending approval hold too — leaving it live would let
		// the SAME hold match the user's next approve/deny reply
		// and trap them in a repeat-denial loop on a drift that's
		// already closed.
		driftID := ""
		if action.Hold != nil {
			driftID = action.Hold.ScopeDriftID
		}
		logger := req.Logger
		if logger == nil {
			logger = slog.Default()
		}
		if driftID != "" && req.ScopeDrifts != nil {
			if denyErr := req.ScopeDrifts.SetOutcome(ctx, driftID, ScopeDriftOutcomeDenied); denyErr != nil {
				logger.WarnContext(ctx, "scope-drift body-rewrite-unsupported denied write failed; drift will TTL out",
					"drift_id", driftID, "err", denyErr)
			}
		}
		if dropErr := req.PendingApproval.Drop(ctx, ResolveRequest{
			UserID:         req.Agent.UserID,
			AgentID:        req.Agent.ID,
			Provider:       req.Provider,
			ConversationID: req.ConversationID,
			ApprovalID:     action.Hold.ID,
		}); dropErr != nil {
			logger.WarnContext(ctx, "scope-drift hold drop failed after body-rewrite-unsupported; hold will TTL out",
				"approval_id", action.Hold.ID, "err", dropErr)
		}
		out.Decision = "deny"
		out.Outcome = "scope_drift_body_rewrite_unsupported"
		out.Reason = "could not rewrite user message in current request body shape"
		out.DriftID = driftID
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

	// Decide the would-be outcome and compute the replacement up front
	// so the SetOutcome and body rewrite below operate on a single
	// pre-committed plan.
	var (
		replacement     string
		intendedOutcome ScopeDriftOutcome
	)
	if verb == "approve" {
		intendedOutcome = ScopeDriftOutcomeSucceeded
		out.Decision = "allow"
		out.Outcome = "scope_drift_one_off_approved"
		replacement = "[Clawvisor scope-drift] Your one-off approval landed for drift " + resolved.ScopeDriftID + ". Re-emit the original tool call unchanged — Clawvisor pre-clears it once on this drift_id."
	} else {
		intendedOutcome = ScopeDriftOutcomeDenied
		out.Decision = "deny"
		out.Outcome = "scope_drift_one_off_denied"
		replacement = "[Clawvisor scope-drift] The one-off was denied. This drift_id is now closed. Do not retry under it — re-emit the original tool call only after you have a new plan (a fresh expand, a new task, or a different approach)."
	}

	// Write the registry outcome BEFORE the body rewrite. SetOutcome
	// is what mints the pre-clear on Succeeded; sequencing the body
	// rewrite after it means a successful body claiming "pre-clear is
	// ready" can never land without the pre-clear actually existing.
	// The earlier order had the opposite failure mode (body says
	// success, no pre-clear; agent retries and gets blocked again
	// with no clear explanation).
	//
	// If SetOutcome fails on the success path, downgrade to a denial
	// in both the metadata and the user-facing body so the agent
	// doesn't get a stale "approval landed" message backed by no
	// pre-clear. On the deny path a SetOutcome failure just means the
	// drift will TTL out as unresolved — non-fatal.
	if err := req.ScopeDrifts.SetOutcome(ctx, resolved.ScopeDriftID, intendedOutcome); err != nil {
		logger.ErrorContext(ctx, "scope-drift outcome write failed before body rewrite",
			"drift_id", resolved.ScopeDriftID, "intended_outcome", intendedOutcome, "err", err)
		if intendedOutcome == ScopeDriftOutcomeSucceeded {
			intendedOutcome = ScopeDriftOutcomeDenied
			out.Decision = "deny"
			out.Outcome = "scope_drift_pre_clear_failed"
			out.Reason = err.Error()
			replacement = "[Clawvisor scope-drift] Your approval landed, but Clawvisor could not record the pre-clear (" + err.Error() + "). This drift_id is now closed — re-emit the original tool call to start over with a fresh menu."
			if denyErr := req.ScopeDrifts.SetOutcome(ctx, resolved.ScopeDriftID, ScopeDriftOutcomeDenied); denyErr != nil {
				logger.WarnContext(ctx, "scope-drift post-failure denied write also failed; drift will TTL out",
					"drift_id", resolved.ScopeDriftID, "err", denyErr)
			}
		}
	}

	rewritten, ok, err := editor.ReplaceLatestUserText(verb, resolved.ID, replacement, nil)
	if err != nil {
		// Body rewrite errored after the registry already committed.
		// The agent will see the bare verb upstream. On the Succeeded
		// path that's fine — the pre-clear is real and the retry
		// will pass. On the Denied path it's also fine — the model
		// reading bare "no" recovers reasonably.
		logger.WarnContext(ctx, "scope-drift body rewrite failed after registry committed",
			"drift_id", resolved.ScopeDriftID, "outcome", intendedOutcome, "err", err)
		return out, err
	}
	if !ok {
		// Rewrite probe passed but the actual replacement is
		// unsupported by the current body shape. Same reasoning as
		// the error branch above — registry already reflects the
		// truth, bare verb upstream is recoverable.
		logger.WarnContext(ctx, "scope-drift body rewrite returned not-ok after registry committed",
			"drift_id", resolved.ScopeDriftID, "outcome", intendedOutcome)
		out.Decision = "deny"
		out.Outcome = "scope_drift_body_rewrite_unsupported"
		out.Reason = "body shape changed between probe and rewrite"
		return out, nil
	}
	out.Body = rewritten
	out.Rewritten = true
	return out, nil
}
