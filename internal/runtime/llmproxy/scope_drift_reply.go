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
		// The drift in the registry would otherwise sit at
		// ChosenOption=one_off / Outcome=pending until TTL,
		// masking a real failure as "still waiting for the
		// user." Flip it to Denied so a status poll surfaces the
		// dead-end. Drop the pending approval hold too — leaving
		// it live would let the SAME hold match the user's next
		// approve/deny reply and trap them in a repeat-denial
		// loop on a drift that's already closed.
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

	// Build the replacement and decide the would-be outcome BEFORE
	// the final body rewrite. We defer the SetOutcome write until
	// after ReplaceLatestUserText succeeds so the drift's terminal
	// state reflects what actually landed on the wire — if the
	// rewrite fails (a rare race between probe and final rewrite),
	// the model would otherwise see a bare "yes" while the drift
	// claimed "approved", and the agent's retry would consume a
	// pre-clear that the user never actually saw approved.
	var (
		replacement   string
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

	rewritten, ok, err := editor.ReplaceLatestUserText(verb, resolved.ID, replacement)
	if err != nil {
		// Final rewrite failed mid-flight. Hold is consumed; flip
		// the drift to Denied so it isn't stranded at pending. The
		// model will see the bare verb in the body — annoying but
		// not stuck — and the agent's poll/retry shows the drift
		// terminal.
		if denyErr := req.ScopeDrifts.SetOutcome(ctx, resolved.ScopeDriftID, ScopeDriftOutcomeDenied); denyErr != nil {
			logger.WarnContext(ctx, "scope-drift outcome denied write failed after rewrite error; drift will TTL out",
				"drift_id", resolved.ScopeDriftID, "err", denyErr)
		}
		return out, err
	}
	if !ok {
		// Rewrite was unsupported by the body shape between probe
		// and final call (rare; the probe just passed). Same
		// reasoning as the error branch above — close the drift so
		// it's not stranded.
		if denyErr := req.ScopeDrifts.SetOutcome(ctx, resolved.ScopeDriftID, ScopeDriftOutcomeDenied); denyErr != nil {
			logger.WarnContext(ctx, "scope-drift outcome denied write failed after rewrite returned not-ok; drift will TTL out",
				"drift_id", resolved.ScopeDriftID, "err", denyErr)
		}
		out.Decision = "deny"
		out.Outcome = "scope_drift_body_rewrite_unsupported"
		out.Reason = "body shape changed between probe and rewrite"
		return out, nil
	}

	// Rewrite committed. NOW write the drift outcome — on approve,
	// this is the SetOutcome(Succeeded) that inserts the one-shot
	// pre-clear keyed by (agent, fingerprint) so the agent's next
	// attempt of the original blocked tool_use passes scope+intent
	// verification. On deny, this is the SetOutcome(Denied) that
	// surfaces the terminal state to status pollers.
	if err := req.ScopeDrifts.SetOutcome(ctx, resolved.ScopeDriftID, intendedOutcome); err != nil {
		// Body is already rewritten; the model will see the
		// success/denial message but the registry write failed.
		// On the succeed path this means the agent's retry will
		// re-block at the same drift; on deny it just means a
		// later poll reports pending until TTL. Either way, log
		// loudly and best-effort-Deny so the drift can't be left
		// claiming "succeeded" when the pre-clear never landed.
		logger.ErrorContext(ctx, "scope-drift outcome write failed after body rewrite committed",
			"drift_id", resolved.ScopeDriftID, "intended_outcome", intendedOutcome, "err", err)
		if intendedOutcome == ScopeDriftOutcomeSucceeded {
			if denyErr := req.ScopeDrifts.SetOutcome(ctx, resolved.ScopeDriftID, ScopeDriftOutcomeDenied); denyErr != nil {
				logger.WarnContext(ctx, "scope-drift post-failure denied write also failed; drift will TTL out",
					"drift_id", resolved.ScopeDriftID, "err", denyErr)
			}
			out.Decision = "deny"
			out.Outcome = "scope_drift_pre_clear_failed"
			out.Reason = err.Error()
		}
	}
	out.Body = rewritten
	out.Rewritten = true
	return out, nil
}
