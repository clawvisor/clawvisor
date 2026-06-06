package llmproxy

import (
	"context"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/inspector"
	runtimedecision "github.com/clawvisor/clawvisor/pkg/runtime/decision"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// evalCapture records one tool_use's outcome from the first eval pass.
// Captured for every tool_use in a turn so the coalesce decision can
// classify the whole turn before the response body is finalized.
type evalCapture struct {
	Use         conversation.ToolUse
	Kind        HeldToolUseKind
	HoldID      string
	Stage       PendingApprovalStage
	Inspector   inspector.Verdict
	Fingerprint runtimedecision.DecisionFingerprint
	Reason      string
	// TaskID names the active task this tool_use matched (via task
	// scope or authorization decision), when one was matched. Empty
	// for paths that ran before/without a task match (trigger miss
	// pass-through, control-tool inline-task creation, hard denials
	// preceding any decision). Forwarded to the coalesced audit rows
	// so dashboards filtering on task_id surface coalesced-pending
	// rows alongside their owning task.
	TaskID string
}

// capturedHoldSink buffers PendingApprovalCache.Hold calls the first
// eval pass makes. The wrapper does NOT touch the underlying cache
// during pass 1 — it generates a stable ID, stores the Pending in the
// buffer, and returns. After the coalesce decision the buffer is
// either replayed into the underlying cache (legacy mode) or
// discarded in favor of one coalesced hold (coalesce mode).
//
// Buffering avoids two distinct hazards that passthrough has:
//   - Misleading audit ("allow"/"rewrite" rows for siblings whose
//     calls were actually replaced by a coalesced approval prompt).
//     The audit emitter is buffered with the same lifecycle so the
//     coalesce branch can suppress those rows and replace them with
//     "coalesced_approval_pending" rows that match what actually
//     happened.
//   - Spurious eviction in bounded caches. If the cache is near
//     capacity, inserting N per-tool holds then the coalesced hold
//     then dropping the per-tool holds can evict an unrelated older
//     approval that nobody intended to displace. Buffering keeps the
//     underlying cache untouched until the final shape is decided.
type capturedHoldSink struct {
	holds []capturedHold
}

type capturedHold struct {
	// Pending carries the full PendingLiteApproval that the caller
	// passed to Hold. ID/CreatedAt/ExpiresAt are populated by the
	// wrapper so callers that consume the returned ID immediately
	// (e.g. the inline-task intercept building its substitute text)
	// get a stable value. Replay re-passes this struct unchanged to
	// the underlying cache.
	Pending PendingLiteApproval
}

// holdCapturingApprovalCache wraps a PendingApprovalCache so pass-1
// Hold calls are buffered, not committed. Peek/Resolve/Drop fall
// through to the inner cache so the eval pass can still observe
// existing state (e.g. the inline-task two-step flow checking for an
// older awaiting-definition hold).
type holdCapturingApprovalCache struct {
	inner PendingApprovalCache
	sink  *capturedHoldSink
}

func newHoldCapturingApprovalCache(inner PendingApprovalCache, sink *capturedHoldSink) *holdCapturingApprovalCache {
	return &holdCapturingApprovalCache{
		inner: inner,
		sink:  sink,
	}
}

func (c *holdCapturingApprovalCache) Hold(_ context.Context, pending PendingLiteApproval) (HoldResult, error) {
	if pending.ID == "" {
		id, err := newLiteApprovalID()
		if err != nil {
			return HoldResult{}, err
		}
		pending.ID = id
	}
	// Deliberately do NOT fabricate CreatedAt or ExpiresAt here. The
	// underlying cache's Hold sets them from its own configured TTL,
	// and overwriting with a wrapper-local default would bypass that
	// configuration — a memory cache instantiated with
	// NewMemoryPendingApprovalCache(time.Minute) would end up
	// granting 10-minute holds via the wrapper, etc. Callers that
	// need a specific TTL (e.g. inline-task with its own 10m window)
	// set ExpiresAt explicitly; we preserve that. Callers that don't
	// set it get whatever the underlying cache decides at replay.
	if c.sink != nil {
		c.sink.holds = append(c.sink.holds, capturedHold{Pending: pending})
	}
	// Evicted is reported only at replay time, when the real cache
	// actually inserts and may displace an older entry. Returning nil
	// here matches reality (nothing has been inserted yet) and keeps
	// the per-tool eviction audit row honest: if there's no eviction
	// because there's no insert, there's no row.
	return HoldResult{Pending: pending}, nil
}

func (c *holdCapturingApprovalCache) Peek(ctx context.Context, req ResolveRequest) (*PendingLiteApproval, error) {
	return c.inner.Peek(ctx, req)
}

func (c *holdCapturingApprovalCache) Resolve(ctx context.Context, req ResolveRequest) (*PendingLiteApproval, error) {
	return c.inner.Resolve(ctx, req)
}

func (c *holdCapturingApprovalCache) Drop(ctx context.Context, req ResolveRequest) error {
	return c.inner.Drop(ctx, req)
}

// capturedAuditSink buffers audit rows from pass 1. See capturedHoldSink
// for the motivation: pass 1 may emit "allow"/"rewrite" rows for
// siblings whose calls are then replaced by a coalesced approval, so
// dashboards keyed on those rows would believe the calls executed.
// Buffering lets the coalesce branch suppress them and emit corrected
// rows instead.
type capturedAuditSink struct {
	entries []BufferedAudit
}

// store.Agent is referenced only by the helper signatures below.

// replayBufferedHolds writes the buffered per-tool holds to the
// underlying cache. Used on the legacy path (no coalescence) and on
// the coalesced-Hold-failure fallback. Atomicity: if any single Hold
// fails, every previously-written hold from this batch is dropped
// and a non-nil error is returned so the caller (Postprocess) can
// fail closed for the whole response — the alternative is shipping
// an approval prompt that references a hold that doesn't exist,
// which leaves the user typing "yes" into a void.
//
// Eviction is audited per-row since it didn't happen in pass 1 (the
// wrapper deferred all writes).
func replayBufferedHolds(ctx context.Context, cfg PostprocessConfig, inner PendingApprovalCache, sink *capturedHoldSink, agent *store.Agent, captures []evalCapture) error {
	if inner == nil || sink == nil || len(sink.holds) == 0 {
		return nil
	}
	// Track every successful commit so we can undo on the first
	// failure. Drop is best-effort (TTL ages anything we miss out
	// eventually); the important thing is that the response body we
	// return reflects "no holds exist" rather than "some holds exist."
	committed := make([]string, 0, len(sink.holds))
	for i, h := range sink.holds {
		res, err := inner.Hold(ctx, h.Pending)
		if err != nil {
			for _, id := range committed {
				_ = inner.Drop(ctx, ResolveRequest{
					UserID:     h.Pending.UserID,
					AgentID:    h.Pending.AgentID,
					Provider:   h.Pending.Provider,
					ApprovalID: id,
				})
			}
			if cfg.Audit != nil && agent != nil && i < len(captures) {
				use := captures[i].Use
				v := captures[i].Inspector
				cfg.Audit.LogToolUseInspected(ctx, agent, cfg.RequestID, use, v, "block", "approval_hold_replay_failed", err.Error(), captures[i].TaskID)
			}
			return err
		}
		committed = append(committed, res.Pending.ID)
		if res.Evicted != nil {
			if cfg.Audit != nil && agent != nil && i < len(captures) {
				use := captures[i].Use
				v := captures[i].Inspector
				cfg.Audit.LogToolUseInspected(ctx, agent, cfg.RequestID, use, v, "block", "approval_evicted", "superseded pending approval "+res.Evicted.ID, captures[i].TaskID)
			}
			// If the evicted hold was an inline-task one, flip its
			// store.Task row to "expired" so the dashboard stops
			// showing the now-unresolvable "reply in chat" guidance.
			// Safe no-op for non-inline evictions.
			cleanupEvictedInlineTask(ctx, cfg, res.Evicted)
		}
	}
	return nil
}

// flushBufferedAudit emits each buffered audit row to the configured
// audit emitter. Used on the legacy path; coalesce mode replaces this
// with emitCoalescedPendingAuditRows.
func flushBufferedAudit(ctx context.Context, cfg PostprocessConfig, agent *store.Agent, sink *capturedAuditSink) {
	if cfg.Audit == nil || agent == nil || sink == nil {
		return
	}
	for _, e := range sink.entries {
		cfg.Audit.LogToolUseInspected(ctx, agent, cfg.RequestID, e.ToolUse, e.Verdict, e.Decision, e.Outcome, e.Reason, e.TaskID)
	}
}

// emitCoalescedPendingAuditRows replaces the buffered audit with one
// "coalesced_approval_pending" row per held tool_use. The original
// classification (allow/rewrite/approval) is recorded in the reason
// string so the trail still answers "what would have happened without
// coalescence?" but the decision/outcome reflect what actually
// happened: the call did not execute and is awaiting one combined
// user approval.
//
// The audit schema's UNIQUE(user_id, request_id, COALESCE(task_id,''))
// canonical dedup index collapses repeated emits for one request to a
// single persisted row. Approval-triggering captures are emitted
// FIRST so the row that wins dedup describes the call that drove the
// hold — without ordering, an auto-allow sibling that happens to be
// first in turn order would shadow the approval-needing call in the
// persisted trail.
func emitCoalescedPendingAuditRows(ctx context.Context, cfg PostprocessConfig, agent *store.Agent, captures []evalCapture, approvalID string) {
	if cfg.Audit == nil || agent == nil {
		return
	}
	ordered := make([]evalCapture, 0, len(captures))
	for _, c := range captures {
		if c.Kind == HeldKindApproval {
			ordered = append(ordered, c)
		}
	}
	for _, c := range captures {
		if c.Kind != HeldKindApproval {
			ordered = append(ordered, c)
		}
	}
	for _, c := range ordered {
		reason := "held under coalesced approval " + approvalID + " (originally classified as " + string(c.Kind) + ")"
		cfg.Audit.LogToolUseInspected(ctx, agent, cfg.RequestID, c.Use, c.Inspector, "block", "coalesced_approval_pending", reason, c.TaskID)
	}
}

// classifyVerdict infers the held-use kind from a verdict. The
// distinction Postprocess actually needs is approval-vs-not, but
// allow/rewrite/deny are tracked so the coalesced prompt can label
// each held use accurately ("auto-allowed alongside"/"rewritten
// alongside"). The reason-string match on "approval required" is the
// agreed contract between this classifier and the eval return paths
// — those reasons are constructed in eval at known points and the
// substring is stable.
func classifyVerdict(v conversation.ToolUseVerdict) HeldToolUseKind {
	if v.Allowed {
		if len(v.RewriteInput) > 0 {
			return HeldKindRewrite
		}
		return HeldKindAllow
	}
	// Approval prompts always include "approval required" in the
	// Reason string (built in postprocess.go at the two NeedsApproval
	// branches). Inline-task intercept uses "awaiting inline task
	// approval" — classified by the Stage field on the captured hold,
	// not by this function.
	for _, marker := range []string{"approval required", "awaiting inline task approval"} {
		if containsFold(v.Reason, marker) {
			return HeldKindApproval
		}
	}
	return HeldKindDeny
}

// shouldCoalesceTurn decides whether the post-pass should replace the
// per-tool holds with a single coalesced hold for this turn.
//
// Coalesce iff:
//   - at least one approval-needing tool_use is present, AND
//   - more than one tool_use total (otherwise there is nothing to
//     coalesce — the single hold IS the turn), AND
//   - no inline-task hold is present (stage != StageTool); the inline-
//     task flow is single-tool by design and its hold belongs to a
//     different state machine.
//
// A hard-denied tool_use in the same turn falls back to legacy
// behavior — coalescing around a hard deny would surface a confusing
// "approve to run the not-blocked one" prompt while another sibling
// is permanently blocked.
func shouldCoalesceTurn(captures []evalCapture) bool {
	if len(captures) <= 1 {
		return false
	}
	approvals := 0
	for _, c := range captures {
		switch c.Kind {
		case HeldKindApproval:
			if c.Stage != "" && c.Stage != StageTool {
				return false
			}
			approvals++
		case HeldKindDeny:
			return false
		}
	}
	return approvals >= 1
}

func containsFold(s, substr string) bool {
	if len(substr) == 0 {
		return true
	}
	// ASCII case-insensitive substring search. The reason strings we
	// match against are ASCII-only by construction, so a full Unicode
	// fold isn't required. Avoiding strings.EqualFold + indexing keeps
	// allocations to zero.
	if len(s) < len(substr) {
		return false
	}
	for i := 0; i+len(substr) <= len(s); i++ {
		match := true
		for j := 0; j < len(substr); j++ {
			a := s[i+j]
			b := substr[j]
			if a >= 'A' && a <= 'Z' {
				a += 'a' - 'A'
			}
			if b >= 'A' && b <= 'Z' {
				b += 'a' - 'A'
			}
			if a != b {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
