package pipeline

import (
	"context"
	"errors"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/eval"
)

// Finalizer owns the side-effect buffers + finalize logic that
// previously lived in postproc: per-tool hold captures, audit-event
// buffering, the coalesce-vs-replay decision, hold replay, and
// coalesced audit emission. Postproc now accumulates per-tool data
// via AddHold + AddAudit and calls Finalize at end-of-response.
//
// Finalizer is intentionally single-goroutine: callers must finish
// adding captures/audits before calling Finalize, Rollback, FlushAudits,
// or Captures.
//
// The Finalizer doesn't import llmproxy. Domain-coupled operations
// (building a PendingLiteApproval from buffered captures, formatting
// the approval prompt) flow through FinalizerDeps. Payload values
// (HoldCapture.Payload, HoldSubmitResult.Evicted) are opaque to this
// package — the deps adapter unpacks them at the boundary.
type Finalizer struct {
	deps     FinalizerDeps
	captures []HoldCapture // every tool_use (drives coalesce decision)
	audits   []conversation.AuditEvent
}

// HoldCapture records one tool_use's eval outcome. Every tool_use
// in the response produces a capture; only those that called Hold
// during the eval pass carry a non-nil Payload. The orchestrator's
// coalesce decision iterates the full list (capturing Allow/Rewrite
// siblings so they fold under the coalesced prompt); submit / replay
// skip captures with no payload.
type HoldCapture struct {
	// ToolUse carries the tool_use block this capture covers.
	// Audit/prompt deps read ToolUse.Name and ToolUse.Input from
	// here for captures that didn't Hold (no Payload to unpack).
	ToolUse conversation.ToolUse

	// ToolUseID identifies the tool_use this capture applies to.
	ToolUseID string

	// InspectorSnapshot is the inspector verdict the eval pass
	// produced for this tool_use, projected into the audit-row
	// snapshot type. Carried separately from Payload so Allow /
	// Rewrite siblings (no Payload) can still surface their
	// inspector verdict to audit/prompt rendering.
	InspectorSnapshot conversation.InspectorVerdictSnapshot

	// ApprovalID is the deps-assigned ID for the buffered hold,
	// when one exists. Empty for captures that didn't Hold.
	ApprovalID string

	// Stage tags this hold's stage in the approval flow. The
	// orchestrator coalesces holds where every Stage is the
	// coalescible label ("tool" by convention); non-coalescible
	// stages (e.g. "inline_task") force per-tool replay.
	Stage string

	// Kind is the typed classification from eval.HeldKindHint.
	// Set for every capture so the coalesce decision sees Allow /
	// Rewrite siblings alongside the held Approvals.
	Kind eval.HeldKindHint

	// Payload is the deps-private hold representation when this
	// tool_use buffered a Hold; nil otherwise. Coalesce builders
	// fold non-nil payloads into the merged hold; submit/replay
	// skips captures with nil payload.
	Payload any
}

// Holds returns the subset of c that captured a Hold (Payload != nil).
// Helper for deps that build a coalesced payload from the actually-held
// tool_uses while still seeing the full sibling list for context.
func Holds(captures []HoldCapture) []HoldCapture {
	out := make([]HoldCapture, 0, len(captures))
	for _, c := range captures {
		if c.Payload != nil {
			out = append(out, c)
		}
	}
	return out
}

// HoldSubmitResult is what FinalizerDeps.SubmitHold returns. Evicted
// is the optional previously-pending payload the deps wants to clean
// up (the orchestrator passes it back through Cleanup). ApprovalID
// names the newly committed hold; EvictedApprovalID names the
// displaced hold when Evicted is non-nil.
type HoldSubmitResult struct {
	ApprovalID        string
	EvictedApprovalID string
	Evicted           any
}

// CoalescedHold is what FinalizerDeps.BuildCoalescedHold returns. The
// orchestrator commits Payload via SubmitHold; the per-tool audit
// + prompt strings come from PerToolAudit + Prompt.
type CoalescedHold struct {
	// Payload is the deps-private representation of the coalesced
	// hold (typically a single llmproxy.PendingLiteApproval
	// covering every captured tool_use).
	Payload any

	// EvictedAuditFor formats the audit row when SubmitHold
	// reports an evicted prior hold. Receives the first capture
	// (the primary tool_use) + the evicted approval ID.
	EvictedAuditFor func(primary HoldCapture, evictedID string) conversation.AuditEvent

	// PerToolAuditFor formats the per-tool "coalesced_approval_pending"
	// audit row for each captured tool_use after the coalesced hold
	// commits.
	PerToolAuditFor func(capture HoldCapture, approvalID string) conversation.AuditEvent

	// Prompt renders the human-facing approval prompt that replaces
	// the assistant's content when the coalesced hold commits.
	Prompt func(approvalID string) string
}

// FinalizerDeps abstracts the side effects the Finalizer commits.
// Implemented by llmproxy at the package boundary so pipeline doesn't
// import the domain types.
type FinalizerDeps interface {
	// SubmitHold commits a buffered hold to durable storage. Used
	// for both the replay path (per-tool) and the coalesced path
	// (single hold supersedes all captures).
	SubmitHold(ctx context.Context, payload any) (HoldSubmitResult, error)

	// DropHold rolls back a previously-committed hold. Called on
	// partial replay failure so the partially-committed batch is
	// fully unwound.
	DropHold(ctx context.Context, capture HoldCapture) error

	// BuildCoalescedHold constructs the single hold supersedes
	// multiple buffered captures. Returns the coalesced payload
	// + the audit/prompt formatters the orchestrator uses to
	// emit per-tool audit rows + the coalesced prompt.
	BuildCoalescedHold(captures []HoldCapture) CoalescedHold

	// BuildReplayFailedAudit formats the audit row when a single
	// replay submit fails.
	BuildReplayFailedAudit(capture HoldCapture, err error) conversation.AuditEvent

	// BuildEvictedAudit formats the audit row when the per-tool
	// replay path sees an evicted prior hold from a single submit.
	BuildEvictedAudit(capture HoldCapture, evictedID string) conversation.AuditEvent

	// CleanupEvictedHold runs any deps-private cleanup on a hold
	// the submit displaced (e.g. expiring an inline-task row).
	CleanupEvictedHold(ctx context.Context, evicted any)

	// RollbackPendingTask expires any pending inline-task row a
	// buffered hold created when the turn fails before commit.
	RollbackPendingTask(ctx context.Context, capture HoldCapture)

	// WriteAudit emits a typed audit event to the audit store.
	WriteAudit(ctx context.Context, ev conversation.AuditEvent)
}

// NewFinalizer constructs a Finalizer wired to the supplied deps.
func NewFinalizer(deps FinalizerDeps) *Finalizer {
	return &Finalizer{deps: deps}
}

// AddCapture records a per-tool eval capture. Captures with non-nil
// Payload represent buffered holds; captures with nil Payload
// represent Allow/Rewrite siblings that influence the coalesce
// decision without contributing to the replay batch.
func (f *Finalizer) AddCapture(capture HoldCapture) {
	if f == nil {
		return
	}
	f.captures = append(f.captures, capture)
}

// AddAudit buffers an audit event the eval pass emitted. Buffered
// events flush at Finalize time on the per-tool replay path, or are
// replaced with per-tool coalesced rows on the coalesce path.
func (f *Finalizer) AddAudit(ev conversation.AuditEvent) {
	if f == nil {
		return
	}
	f.audits = append(f.audits, ev)
}

// FinalizeResult carries the orchestrator's end-of-response decision.
type FinalizeResult struct {
	// Coalesced is true when the orchestrator replaced per-tool
	// holds with one coalesced hold for the turn.
	Coalesced bool

	// CoalescedApprovalID names the single hold the coalesce path
	// committed. Empty on the per-tool replay path.
	CoalescedApprovalID string

	// CoalescedPrompt is the human-facing approval-prompt text the
	// caller injects into the assistant's response, replacing the
	// model's content. Empty on the per-tool replay path.
	CoalescedPrompt string

	// CoalescedCapture is the committed coalesced hold. Callers can use
	// it to roll back the hold if rendering the coalesced prompt fails
	// after storage has already committed.
	CoalescedCapture *HoldCapture
}

// Finalize commits buffered holds + audits. Returns (Coalesced, ...)
// telling the caller whether to swap in the coalesced prompt at the
// rewriter step.
func (f *Finalizer) Finalize(ctx context.Context) (FinalizeResult, error) {
	if f == nil || f.deps == nil {
		return FinalizeResult{}, nil
	}
	defer f.flushAudits(ctx)
	if shouldCoalesce(f.captures) {
		result, err := f.commitCoalesced(ctx)
		if err != nil {
			f.flushAudits(ctx)
			return FinalizeResult{}, err
		}
		f.audits = nil
		return result, nil
	}
	if err := f.replayLegacy(ctx); err != nil {
		return FinalizeResult{}, err
	}
	return FinalizeResult{}, nil
}

// Rollback expires any pending inline-task rows the buffered captures
// created. Called when the response is being abandoned before
// Finalize (e.g., rewriter mid-stream error). Does NOT submit any
// buffered hold to durable storage.
func (f *Finalizer) Rollback(ctx context.Context) {
	if f == nil || f.deps == nil {
		return
	}
	f.rollbackPendingTasks(ctx, f.captures)
	f.flushAudits(ctx)
}

// FlushAudits emits the buffered audit events without changing hold
// state. Used by callers that ran replayLegacy themselves or that
// need to flush after a coalesced-commit failure recovery.
//
// Exposed because the streaming + buffered paths take different
// fast-fail paths between hold replay and audit flush.
func (f *Finalizer) FlushAudits(ctx context.Context) {
	if f == nil || f.deps == nil {
		return
	}
	f.flushAudits(ctx)
}

// DropCommittedHold removes a hold that Finalize already committed.
// It is intended for caller-side failure after commit, such as a
// coalesced prompt render/rewrite failure.
func (f *Finalizer) DropCommittedHold(ctx context.Context, capture HoldCapture) error {
	if f == nil || f.deps == nil {
		return nil
	}
	return f.deps.DropHold(ctx, capture)
}

// HasCoalesceCandidates reports whether shouldCoalesce would fire on
// the current captures. Callers use this for pre-commit branching
// (e.g., to choose a rewriter callback shape) without committing
// the decision.
func (f *Finalizer) HasCoalesceCandidates() bool {
	if f == nil {
		return false
	}
	return shouldCoalesce(f.captures)
}

// Captures returns a shallow copy of the buffered hold-capture slice.
// Payload values are shared with the Finalizer.
// Stream code uses this to drive per-tool wire-format emission.
func (f *Finalizer) Captures() []HoldCapture {
	if f == nil {
		return nil
	}
	return append([]HoldCapture(nil), f.captures...)
}

func (f *Finalizer) commitCoalesced(ctx context.Context) (FinalizeResult, error) {
	coalesced := f.deps.BuildCoalescedHold(f.captures)
	ordered := orderCapturesForCoalescedAudit(f.captures)
	if len(ordered) > 0 && coalesced.PerToolAuditFor == nil {
		f.flushAudits(ctx)
		return FinalizeResult{}, errors.New("pipeline.Finalizer: coalesced hold missing per-tool audit builder")
	}
	submit, err := f.deps.SubmitHold(ctx, coalesced.Payload)
	if err != nil {
		f.rollbackPendingTasks(ctx, f.captures)
		return FinalizeResult{}, err
	}
	if submit.Evicted != nil && len(f.captures) > 0 {
		if len(ordered) > 0 {
			primary := ordered[0]
			if coalesced.EvictedAuditFor != nil {
				f.deps.WriteAudit(ctx, coalesced.EvictedAuditFor(primary, submit.EvictedApprovalID))
			}
		}
		f.deps.CleanupEvictedHold(ctx, submit.Evicted)
	}
	for _, c := range ordered {
		f.deps.WriteAudit(ctx, coalesced.PerToolAuditFor(c, submit.ApprovalID))
	}
	prompt := ""
	if coalesced.Prompt != nil {
		prompt = coalesced.Prompt(submit.ApprovalID)
	}
	committedCapture := HoldCapture{
		Kind:       eval.HeldKindHintApproval,
		Payload:    coalesced.Payload,
		ApprovalID: submit.ApprovalID,
	}
	return FinalizeResult{
		Coalesced:           true,
		CoalescedApprovalID: submit.ApprovalID,
		CoalescedPrompt:     prompt,
		CoalescedCapture:    &committedCapture,
	}, nil
}

func (f *Finalizer) replayLegacy(ctx context.Context) error {
	committed := make([]HoldCapture, 0, len(f.captures))
	for i, c := range f.captures {
		if c.Payload == nil {
			// Allow/Rewrite sibling — no hold buffered for this
			// tool_use. Skip the replay submit; it still
			// contributed to the coalesce-decision input.
			continue
		}
		res, err := f.deps.SubmitHold(ctx, c.Payload)
		if err != nil {
			var rollbackErr error
			for _, prev := range committed {
				rollbackErr = errors.Join(rollbackErr, f.deps.DropHold(ctx, prev))
			}
			if rollbackErr != nil {
				err = errors.Join(err, rollbackErr)
			}
			f.rollbackPendingTasks(ctx, f.captures[i:])
			f.flushAudits(ctx)
			f.deps.WriteAudit(ctx, f.deps.BuildReplayFailedAudit(c, err))
			return err
		}
		if res.Evicted != nil {
			f.deps.WriteAudit(ctx, f.deps.BuildEvictedAudit(c, res.EvictedApprovalID))
			f.deps.CleanupEvictedHold(ctx, res.Evicted)
		}
		c.ApprovalID = res.ApprovalID
		committed = append(committed, c)
	}
	return nil
}

func (f *Finalizer) rollbackPendingTasks(ctx context.Context, captures []HoldCapture) {
	for _, c := range captures {
		f.deps.RollbackPendingTask(ctx, c)
	}
}

func (f *Finalizer) flushAudits(ctx context.Context) {
	for _, ev := range f.audits {
		f.deps.WriteAudit(ctx, ev)
	}
	f.audits = nil
}

// shouldCoalesce decides whether the post-pass should replace the
// per-tool holds with one coalesced hold. Coalescing requires at
// least one approval-class capture AND every approval must be on a
// coalescible stage (empty or "tool"). Deny captures suppress
// coalescing — the user shouldn't be prompted for an approval that
// covers a definite-deny call.
func shouldCoalesce(captures []HoldCapture) bool {
	if len(captures) <= 1 {
		return false
	}
	approvals := 0
	for _, c := range captures {
		switch c.Kind {
		case eval.HeldKindHintApproval:
			if c.Stage != "" && c.Stage != "tool" {
				return false
			}
			if c.Payload != nil {
				approvals++
			}
		case eval.HeldKindHintDeny:
			return false
		}
	}
	return approvals >= 1
}

// orderCapturesForCoalescedAudit puts approval-class captures first
// so the audit row that wins dedup describes the call that drove the
// hold.
func orderCapturesForCoalescedAudit(captures []HoldCapture) []HoldCapture {
	ordered := make([]HoldCapture, 0, len(captures))
	for _, c := range captures {
		if c.Kind == eval.HeldKindHintApproval {
			ordered = append(ordered, c)
		}
	}
	for _, c := range captures {
		if c.Kind != eval.HeldKindHintApproval {
			ordered = append(ordered, c)
		}
	}
	return ordered
}

// ClassifyVerdict maps a per-tool verdict into the typed
// HeldKindHint the orchestrator coalesces over.
//
// Hold-emitting policies set HeldKindHint explicitly. Other verdicts
// classify from the Allowed / RewriteInput shape.
func ClassifyVerdict(v ToolUseVerdict) eval.HeldKindHint {
	if v.HeldKindHint != "" {
		return v.HeldKindHint
	}
	if v.Allowed {
		if len(v.RewriteInput) > 0 {
			return eval.HeldKindHintRewrite
		}
		return eval.HeldKindHintAllow
	}
	return eval.HeldKindHintDeny
}
