// Package postproc owns the response-side orchestration that
// llmproxy's tool_use evaluator chain runs inside: response parsing,
// rewriter invocation, hold buffering, coalesce decision, audit replay,
// and streaming SSE injection. The handler calls postproc.Postprocess
// (buffered) or postproc.PostprocessStream (SSE) once per upstream
// response.
//
// The llmproxy package owns the per-stage evaluators (via the
// policies + pipeline + pipelineeval packages it co-hosts) and the
// exported helper functions (EvaluateTriggerMissAuthorization,
// EvaluateCredentialedAuthorization, MaybeInterceptInlineTaskDefinition).
// postproc imports llmproxy for those building blocks; the dependency
// only flows one way.
package postproc

import (
	"context"
	"time"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/inspector"
	runtimedecision "github.com/clawvisor/clawvisor/pkg/runtime/decision"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// evalCapture records one tool_use's outcome from the first eval pass.
// Captured for every tool_use in a turn so the coalesce decision can
// classify the whole turn before the response body is finalized.
type evalCapture struct {
	Use         conversation.ToolUse
	Kind        llmproxy.HeldToolUseKind
	HoldID      string
	Stage       llmproxy.PendingApprovalStage
	Inspector   inspector.Verdict
	Fingerprint runtimedecision.DecisionFingerprint
	Reason      string
	// TaskID names the active task this tool_use matched (via task
	// scope or authorization decision), when one was matched. Empty
	// for paths that ran before/without a task match.
	TaskID string
}

// capturedHoldSink buffers PendingApprovalCache.Hold calls the first
// eval pass makes. The wrapper does NOT touch the underlying cache
// during pass 1 — it generates a stable ID, stores the Pending in the
// buffer, and returns. After the coalesce decision the buffer is
// either replayed into the underlying cache (legacy mode) or
// discarded in favor of one coalesced hold (coalesce mode).
type capturedHoldSink struct {
	holds []capturedHold
}

type capturedHold struct {
	Pending llmproxy.PendingLiteApproval
}

// holdCapturingApprovalCache wraps a PendingApprovalCache so pass-1
// Hold calls are buffered, not committed. Peek/Resolve/Drop fall
// through to the inner cache.
type holdCapturingApprovalCache struct {
	inner llmproxy.PendingApprovalCache
	sink  *capturedHoldSink
}

func newHoldCapturingApprovalCache(inner llmproxy.PendingApprovalCache, sink *capturedHoldSink) *holdCapturingApprovalCache {
	return &holdCapturingApprovalCache{
		inner: inner,
		sink:  sink,
	}
}

func (c *holdCapturingApprovalCache) Hold(_ context.Context, pending llmproxy.PendingLiteApproval) (llmproxy.HoldResult, error) {
	if pending.ID == "" {
		id, err := llmproxy.NewLiteApprovalID()
		if err != nil {
			return llmproxy.HoldResult{}, err
		}
		pending.ID = id
	}
	if c.sink != nil {
		c.sink.holds = append(c.sink.holds, capturedHold{Pending: pending})
	}
	return llmproxy.HoldResult{Pending: pending}, nil
}

func (c *holdCapturingApprovalCache) Peek(ctx context.Context, req llmproxy.ResolveRequest) (*llmproxy.PendingLiteApproval, error) {
	return c.inner.Peek(ctx, req)
}

func (c *holdCapturingApprovalCache) Resolve(ctx context.Context, req llmproxy.ResolveRequest) (*llmproxy.PendingLiteApproval, error) {
	return c.inner.Resolve(ctx, req)
}

func (c *holdCapturingApprovalCache) Drop(ctx context.Context, req llmproxy.ResolveRequest) error {
	return c.inner.Drop(ctx, req)
}

// capturedAuditSink buffers audit rows from pass 1.
type capturedAuditSink struct {
	entries []llmproxy.BufferedAudit
}

// rollbackBufferedPendingTasks expires any pending inline tasks
// created during the evaluation pass when the turn fails before its
// cache holds are safely committed. The task row is an operational
// orphan in this path, not a user denial, so use ExpireInlineTask
// to match eviction cleanup semantics.
func rollbackBufferedPendingTasks(ctx context.Context, cfg llmproxy.PostprocessConfig, sink *capturedHoldSink) {
	if sink == nil || len(sink.holds) == 0 {
		return
	}
	pendingCreator, ok := cfg.InlineTaskCreator.(llmproxy.InlineTaskPendingCreator)
	if !ok || pendingCreator == nil {
		return
	}
	for _, h := range sink.holds {
		if h.Pending.PendingTaskID == "" || h.Pending.UserID == "" {
			continue
		}
		rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		err := pendingCreator.ExpireInlineTask(rollbackCtx, h.Pending.PendingTaskID, h.Pending.UserID)
		cancel()
		if err != nil && cfg.Trace != nil {
			cfg.Trace.Emit(map[string]any{
				"event":       "inline_task.rollback_expire_failed",
				"request_id":  cfg.RequestID,
				"user_id":     h.Pending.UserID,
				"agent_id":    h.Pending.AgentID,
				"approval_id": h.Pending.ID,
				"task_id":     h.Pending.PendingTaskID,
				"err":         err.Error(),
			})
		}
	}
}

// replayBufferedHolds writes the buffered per-tool holds to the
// underlying cache. Used on the legacy path (no coalescence) and on
// the coalesced-Hold-failure fallback. Atomicity: if any single Hold
// fails, every previously-written hold from this batch is dropped
// and a non-nil error is returned.
func replayBufferedHolds(ctx context.Context, cfg llmproxy.PostprocessConfig, inner llmproxy.PendingApprovalCache, sink *capturedHoldSink, agent *store.Agent, captures []evalCapture) error {
	if inner == nil || sink == nil || len(sink.holds) == 0 {
		return nil
	}
	committed := make([]string, 0, len(sink.holds))
	for i, h := range sink.holds {
		res, err := inner.Hold(ctx, h.Pending)
		if err != nil {
			for _, id := range committed {
				_ = inner.Drop(ctx, llmproxy.ResolveRequest{
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
			llmproxy.CleanupEvictedInlineTask(ctx, cfg, res.Evicted)
		}
	}
	return nil
}

// flushBufferedAudit emits each buffered audit row to the configured
// audit emitter. Used on the legacy path; coalesce mode replaces this
// with emitCoalescedPendingAuditRows.
func flushBufferedAudit(ctx context.Context, cfg llmproxy.PostprocessConfig, agent *store.Agent, sink *capturedAuditSink) {
	if cfg.Audit == nil || agent == nil || sink == nil {
		return
	}
	for _, e := range sink.entries {
		cfg.Audit.LogToolUseInspected(ctx, agent, cfg.RequestID, e.ToolUse, e.Verdict, e.Decision, e.Outcome, e.Reason, e.TaskID)
	}
}

// emitCoalescedPendingAuditRows replaces the buffered audit with one
// "coalesced_approval_pending" row per held tool_use. Approval-triggering
// captures are emitted FIRST so the row that wins dedup describes the
// call that drove the hold.
func emitCoalescedPendingAuditRows(ctx context.Context, cfg llmproxy.PostprocessConfig, agent *store.Agent, captures []evalCapture, approvalID string) {
	if cfg.Audit == nil || agent == nil {
		return
	}
	ordered := make([]evalCapture, 0, len(captures))
	for _, c := range captures {
		if c.Kind == llmproxy.HeldKindApproval {
			ordered = append(ordered, c)
		}
	}
	for _, c := range captures {
		if c.Kind != llmproxy.HeldKindApproval {
			ordered = append(ordered, c)
		}
	}
	for _, c := range ordered {
		reason := "held under coalesced approval " + approvalID + " (originally classified as " + string(c.Kind) + ")"
		cfg.Audit.LogToolUseInspected(ctx, agent, cfg.RequestID, c.Use, c.Inspector, "block", "coalesced_approval_pending", reason, c.TaskID)
	}
}

// classifyVerdict infers the held-use kind from a verdict.
func classifyVerdict(v conversation.ToolUseVerdict) llmproxy.HeldToolUseKind {
	if v.Allowed {
		if len(v.RewriteInput) > 0 {
			return llmproxy.HeldKindRewrite
		}
		return llmproxy.HeldKindAllow
	}
	for _, marker := range []string{"approval required", "awaiting inline task approval"} {
		if containsFold(v.Reason, marker) {
			return llmproxy.HeldKindApproval
		}
	}
	return llmproxy.HeldKindDeny
}

// shouldCoalesceTurn decides whether the post-pass should replace the
// per-tool holds with a single coalesced hold for this turn.
func shouldCoalesceTurn(captures []evalCapture) bool {
	if len(captures) <= 1 {
		return false
	}
	approvals := 0
	for _, c := range captures {
		switch c.Kind {
		case llmproxy.HeldKindApproval:
			if c.Stage != "" && c.Stage != llmproxy.StageTool {
				return false
			}
			approvals++
		case llmproxy.HeldKindDeny:
			return false
		}
	}
	return approvals >= 1
}

func containsFold(s, substr string) bool {
	if len(substr) == 0 {
		return true
	}
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
