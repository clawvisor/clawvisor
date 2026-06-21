package postproc

import (
	"context"
	"net/http"
	"time"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/pipeline"
)

// postprocessSession owns the per-response adapters that bridge
// evaluator side effects into pipeline.Finalizer. Buffered and
// streaming postprocess both use this shape so capture/finalize
// lifecycle details stay in one place.
//
// substitutions tracks pending-substitution registry writes that
// fired during evaluation (scope-drift mints, recoverable-deny
// migrations, inline-task auto-approve). driftOutcomes tracks
// SetOutcome writes deferred via the verdict's DeferredDriftOutcome.
// rollback() iterates both so a request whose response is later
// failClosed'd doesn't leak orphan entries or stale pre-clears.
// commitVerdictSideEffects is the single entry point — it walks each
// verdict, applies the drift outcome first (so the pre-clear lands
// before the substitution mint that depends on it) and then registers
// the substitution. Evaluators MUST NOT call into the registry
// themselves; the spec-on-verdict pattern keeps the verdict pure data
// and concentrates rollback in one place.
type postprocessSession struct {
	baseCfg                  llmproxy.PostprocessConfig
	evalCfg                  llmproxy.PostprocessConfig
	originalPendingApprovals llmproxy.PendingApprovalCache
	holdSink                 *capturedHoldSink
	auditBuf                 *pendingAuditEventBuffer
	finalizer                *pipeline.Finalizer
	fed                      bool
	substitutions            []llmproxy.PendingSubstitutionKey
	driftOutcomes            []string
	// transientConsumed tracks TransientBudget Try() calls the
	// applyTransientTransform wrapper made during eval (via the pure
	// tryPromoteTransient function). rollbackVerdictSideEffects calls
	// TransientBudget.Release on each so a fail-closed response
	// refunds the agent's one retry slot; otherwise the next real
	// attempt would degrade to plain Deny.
	transientConsumed []llmproxy.TransientBudgetKey
}

// promoteTransientsLocked walks verdictByTU and promotes every Deny
// verdict tagged with TransientFailureClass whose budget slot is
// available. Promotion sets RecoverableReason and re-runs the
// placeholder transform so the verdict ends up with the same wire
// shape an evaluator-emitted RecoverableDeny would have produced. The
// consumed budget keys are appended to s.transientConsumed so
// rollbackVerdictSideEffects can refund them if the response later
// fail-closes.
//
// Called from commitVerdictSideEffects BEFORE the existing
// DeferredDriftOutcome / PendingSubstitution processing, so any
// substitution spec the promoted verdict gains is picked up by the
// subsequent registration loop in the same commit pass.
func (s *postprocessSession) promoteTransients(ctx context.Context, verdictByTU map[string]conversation.ToolUseVerdict, toolUses []conversation.ToolUse) {
	if s == nil {
		return
	}
	cfg := s.baseCfg
	for _, tu := range toolUses {
		v, ok := verdictByTU[tu.ID]
		if !ok {
			continue
		}
		promoted, key := tryPromoteTransient(ctx, v, cfg)
		if key == nil {
			continue
		}
		// Re-run the placeholder transform so promoted-transient
		// verdicts emit the same SubstituteWithToolCall +
		// PendingSubstitution shape an evaluator-emitted recoverable
		// would have produced. The subsequent substitution-
		// registration loop in commit handles the new PendingSubstitution.
		promoted = transformRecoverableDenyToPlaceholder(promoted, tu, cfg)
		verdictByTU[tu.ID] = promoted
		s.transientConsumed = append(s.transientConsumed, *key)
	}
}

func newPostprocessSession(cfg llmproxy.PostprocessConfig) *postprocessSession {
	holdSink := &capturedHoldSink{}
	evalCfg := cfg
	originalPendingApprovals := cfg.PendingApprovals
	if originalPendingApprovals != nil {
		evalCfg.PendingApprovals = newHoldCapturingApprovalCache(originalPendingApprovals, holdSink)
	}
	return &postprocessSession{
		baseCfg:                  cfg,
		evalCfg:                  evalCfg,
		originalPendingApprovals: originalPendingApprovals,
		holdSink:                 holdSink,
		auditBuf:                 &pendingAuditEventBuffer{},
		finalizer:                llmproxy.NewFinalizer(cfg, originalPendingApprovals),
	}
}

func (s *postprocessSession) evaluator(req *http.Request, provider conversation.Provider, toolUses []conversation.ToolUse) conversation.ToolUseEvaluator {
	if s == nil {
		return func(conversation.ToolUse) conversation.ToolUseVerdict {
			return conversation.ToolUseVerdict{Allowed: true}
		}
	}
	return selectToolUseEvaluator(req, s.evalCfg, provider, toolUses, s.emitAudit)
}

func (s *postprocessSession) emitAudit(ev conversation.AuditEvent) {
	if s == nil || s.auditBuf == nil {
		return
	}
	s.auditBuf.entries = append(s.auditBuf.entries, ev)
}

func (s *postprocessSession) feed(toolUses []conversation.ToolUse, verdictByTU map[string]conversation.ToolUseVerdict) {
	if s == nil || s.fed {
		return
	}
	s.fed = true
	feedFinalizer(s.finalizer, toolUses, s.holdSink, s.auditBuf, verdictByTU)
}

func (s *postprocessSession) finalize(ctx context.Context, toolUses []conversation.ToolUse, verdictByTU map[string]conversation.ToolUseVerdict) (pipeline.FinalizeResult, error) {
	if s == nil {
		return pipeline.FinalizeResult{}, nil
	}
	s.feed(toolUses, verdictByTU)
	if s.finalizer != nil && s.originalPendingApprovals != nil {
		return s.finalizer.Finalize(ctx)
	}
	flushDirect(ctx, s.baseCfg, s.auditBuf)
	return pipeline.FinalizeResult{}, nil
}

func (s *postprocessSession) rollback(ctx context.Context, toolUses []conversation.ToolUse, verdictByTU map[string]conversation.ToolUseVerdict) {
	if s == nil {
		return
	}
	if s.finalizer != nil {
		s.feed(toolUses, verdictByTU)
		s.finalizer.Rollback(ctx)
	}
	s.rollbackVerdictSideEffects(ctx)
}

// commitVerdictSideEffects walks each verdict in tool_use order and
// realizes the spec-on-verdict signals — promote transients FIRST
// (so any newly-promoted recoverable verdict has its PendingSubstitution
// set before the substitution loop), then DeferredDriftOutcome (so
// the pre-clear mint lands before the substitution write that depends
// on it), then PendingSubstitution. Called BEFORE the real rewrite so
// transient promotions land in the verdict map the rewriter reads.
//
// Ordering is intentional and per-verdict atomic-ish: if drift outcome
// succeeds but substitution fails, the drift outcome is recorded in
// session.driftOutcomes and rolls back via session.rollback when the
// caller fail-closes. The spec's TaskRollback handle (today: only the
// inline-task auto-approve path populates it) is honoured via the
// configured InlineApprovedTaskExpirer with a detached context so a
// mid-request client disconnect doesn't cancel the rollback.
//
// Returns the failing error so the caller (postproc / stream) can
// fail-closed the response — rollback() then sweeps every write made
// on behalf of this request in one place (including transient-budget
// slots refunded via TransientBudget.Release).
func (s *postprocessSession) commitVerdictSideEffects(ctx context.Context, verdictByTU map[string]conversation.ToolUseVerdict, toolUses []conversation.ToolUse) error {
	if s == nil {
		return nil
	}
	// Pass 1: promote transient-deny verdicts. Mutates verdictByTU
	// in-place; tracked consumes go on s.transientConsumed for
	// rollback. Runs regardless of whether ScopeDrifts is wired so
	// the transient mechanism works in deployments that don't use
	// the scope-drift continuation menu.
	s.promoteTransients(ctx, verdictByTU, toolUses)

	registry := s.baseCfg.AuthorizationContext.ScopeDrifts
	if registry == nil {
		return nil
	}
	agentID := s.baseCfg.AgentContext.AgentID
	conversationID := s.baseCfg.AuditContext.ConversationID
	for _, tu := range toolUses {
		v, ok := verdictByTU[tu.ID]
		if !ok {
			continue
		}
		if spec := v.DeferredDriftOutcome; spec != nil && spec.DriftID != "" {
			if err := registry.SetOutcome(ctx, spec.DriftID, llmproxy.ScopeDriftOutcome(spec.Outcome)); err != nil {
				// Roll back the claim the evaluator's guard.Claim made.
				// guard.Success() already fired by the time we got here
				// (the verdict was emitted), so the deferred guard.Rollback
				// in the evaluator is a no-op. The agent's retry should
				// mint a fresh drift; leave this one claimed-but-unresolved
				// would block ClaimOption for the same agent indefinitely.
				_ = registry.RollbackClaim(ctx, spec.DriftID)
				if v.PendingSubstitution != nil && v.PendingSubstitution.TaskRollback != nil {
					s.expireRollbackTask(ctx, v.PendingSubstitution.TaskRollback, err)
				}
				return err
			}
			s.driftOutcomes = append(s.driftOutcomes, spec.DriftID)
		}
		if v.PendingSubstitution == nil {
			continue
		}
		spec := v.PendingSubstitution
		if agentID == "" || conversationID == "" {
			// Identity tuple incomplete — the same guard the evaluators
			// applied before populating the spec. Skip rather than mint
			// a key that would collide across concurrent conversations.
			continue
		}
		key := llmproxy.PendingSubstitutionKey{
			AgentID:        agentID,
			ConversationID: conversationID,
			ToolUseID:      tu.ID,
		}
		err := registry.RegisterPendingSubstitution(ctx, key, llmproxy.PendingSubstitution{
			DriftID:           spec.DriftID,
			MenuText:          spec.MenuText,
			OriginalToolName:  spec.OriginalToolName,
			OriginalToolInput: append([]byte(nil), spec.OriginalToolInput...),
		})
		if err != nil {
			if spec.TaskRollback != nil {
				s.expireRollbackTask(ctx, spec.TaskRollback, err)
			}
			// Earlier drift outcomes / substitutions stay tracked; the
			// session.rollback the caller fires next will sweep them.
			return err
		}
		s.substitutions = append(s.substitutions, key)
	}
	return nil
}

// expireRollbackTask invokes the configured InlineApprovedTaskExpirer
// to unwind an orphan task left behind when registration failed.
// Detached context with a short timeout protects the rollback from a
// canceled client connection (the same condition that may have caused
// the registry write to fail in the first place).
func (s *postprocessSession) expireRollbackTask(ctx context.Context, handle *conversation.PendingSubstitutionTaskRollback, regErr error) {
	creator := s.baseCfg.InlineTaskCreator
	if creator == nil || handle == nil {
		return
	}
	trace := llmproxy.TraceLoggerEmit(s.baseCfg.AuditContext.Trace)
	expirer, ok := creator.(llmproxy.InlineApprovedTaskExpirer)
	if !ok {
		// The creator implementation predates the rollback interface;
		// can't undo. Trace so operators can see why an orphan exists.
		if trace != nil {
			trace("inline_task.auto_approve_rollback_unavailable",
				"task_id", handle.TaskID,
				"reason", "InlineTaskCreator does not implement InlineApprovedTaskExpirer",
			)
		}
		return
	}
	rollbackCtx, cancel := cleanupContext(ctx)
	defer cancel()
	if err := expirer.ExpireInlineApprovedTask(rollbackCtx, handle.TaskID, handle.UserID); err != nil {
		if trace != nil {
			trace("inline_task.auto_approve_rollback_failed",
				"task_id", handle.TaskID,
				"err", err.Error(),
				"register_err", regErr.Error(),
			)
		}
	}
	// The auto-approve evaluator also set the conversation's checkout
	// to the just-expired task. Clear it conditionally — only when the
	// stored value still names OUR task — so a concurrent flow that
	// re-pointed the checkout after we set it isn't clobbered. Without
	// this, subsequent turns surface a "task missing" experience: the
	// model fetches the active task ID, gets the expired one, and asks
	// the user to retry.
	if checkouts := s.baseCfg.Checkouts; checkouts != nil && handle.AgentID != "" && handle.ConversationID != "" {
		key := llmproxy.TaskCheckoutKey{
			UserID:         handle.UserID,
			AgentID:        handle.AgentID,
			ConversationID: handle.ConversationID,
		}
		current, ok, getErr := checkouts.Get(rollbackCtx, key)
		if getErr == nil && ok && current.TaskID == handle.TaskID {
			if clearErr := checkouts.Clear(rollbackCtx, key); clearErr != nil && trace != nil {
				trace("inline_task.auto_approve_rollback_checkout_clear_failed",
					"task_id", handle.TaskID,
					"err", clearErr.Error(),
				)
			}
		}
	}
}

// rollbackVerdictSideEffects undoes every registry write the session
// performed for this request: deferred drift outcomes (via
// RollbackClaim, which also clears the pre-clear minted by
// SetOutcome(Succeeded)) and pending substitutions (via
// DeletePendingSubstitution). Idempotent — subsequent calls are
// no-ops because both slices are cleared.
func (s *postprocessSession) rollbackVerdictSideEffects(ctx context.Context) {
	if s == nil {
		return
	}
	// Refund transient-budget slots regardless of whether the scope-
	// drift registry is wired; the budget is an independent registry
	// and the next real attempt deserves a fresh retry slot.
	if budget := s.baseCfg.AuthorizationContext.TransientBudget; budget != nil {
		for _, key := range s.transientConsumed {
			budget.Release(ctx, key)
		}
	}
	s.transientConsumed = nil

	registry := s.baseCfg.AuthorizationContext.ScopeDrifts
	if registry == nil {
		s.substitutions = nil
		s.driftOutcomes = nil
		return
	}
	for _, key := range s.substitutions {
		registry.DeletePendingSubstitution(ctx, key)
	}
	s.substitutions = nil
	for _, driftID := range s.driftOutcomes {
		// RollbackClaim resets ChosenOption + Outcome AND deletes the
		// pre-clear minted by SetOutcome(Succeeded). The original claim
		// was made by the evaluator (guard.Claim) whose guard.Success()
		// already fired by the time we got here, so the full unwind is
		// the right shape: the agent's retry mints a fresh drift.
		_ = registry.RollbackClaim(ctx, driftID)
	}
	s.driftOutcomes = nil
}

func (s *postprocessSession) dropCommitted(ctx context.Context, capture *pipeline.HoldCapture) error {
	if s == nil || s.finalizer == nil || capture == nil {
		return nil
	}
	cleanupCtx, cancel := cleanupContext(ctx)
	defer cancel()
	return s.finalizer.DropCommittedHold(cleanupCtx, *capture)
}

func (s *postprocessSession) dropCommittedAndRollback(ctx context.Context, capture *pipeline.HoldCapture) error {
	if s == nil {
		return nil
	}
	cleanupCtx, cancel := cleanupContext(ctx)
	defer cancel()
	var err error
	if s.finalizer != nil && capture != nil {
		err = s.finalizer.DropCommittedAndRollback(cleanupCtx, *capture)
	}
	// The streaming write paths call this AFTER commitVerdictSideEffects
	// has already landed substitutions / drift outcomes in the registry.
	// Sweeping the verdict-side-effect writes here too means a partial
	// streaming success (commit succeeded, write failed) doesn't strand
	// the registry with substitutions for tool_uses the harness never
	// actually saw on the wire. Runs unconditional on the session so a
	// nil/no-capture finalizer doesn't short-circuit the verdict-side-
	// effect sweep.
	s.rollbackVerdictSideEffects(cleanupCtx)
	return err
}

func (s *postprocessSession) dropAllCommittedAndRollback(ctx context.Context) error {
	if s == nil {
		return nil
	}
	cleanupCtx, cancel := cleanupContext(ctx)
	defer cancel()
	var err error
	if s.finalizer != nil {
		err = s.finalizer.DropAllCommittedAndRollback(cleanupCtx)
	}
	// Same rationale as dropCommittedAndRollback: any registry writes
	// that landed during commitVerdictSideEffects must be undone when a
	// downstream streaming write fails, or subsequent harness retries
	// will hit stale substitution entries for tool_uses that never
	// reached them.
	s.rollbackVerdictSideEffects(cleanupCtx)
	return err
}

func cleanupContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
}

func (s *postprocessSession) captures() []pipeline.HoldCapture {
	if s == nil || s.finalizer == nil {
		return nil
	}
	return s.finalizer.Captures()
}

// feedFinalizer transfers per-tool eval outcomes + audit events into
// the finalizer. Captures every tool_use (whether or not it called
// Hold) so the coalesce decision sees Allow/Rewrite siblings
// alongside the held Approvals. Captures that didn't Hold carry a
// nil Payload; replay skips them.
//
// orderedToolUses preserves the response order of tool_uses so the
// coalesced primary is selected deterministically + each capture
// carries its ToolUse for audit/prompt rendering.
func feedFinalizer(
	finalizer *pipeline.Finalizer,
	orderedToolUses []conversation.ToolUse,
	holdSink *capturedHoldSink,
	auditBuf *pendingAuditEventBuffer,
	verdictByTU map[string]conversation.ToolUseVerdict,
) {
	if finalizer == nil {
		return
	}
	holdCount := 0
	if holdSink != nil {
		holdCount = len(holdSink.holds)
	}
	holdByTU := make(map[string]capturedHold, holdCount)
	if holdSink != nil {
		for _, h := range holdSink.holds {
			holdByTU[h.Pending.ToolUse.ID] = h
		}
	}
	// Inspector verdicts surface through the buffered audit events
	// the factory emitted. Allow / Rewrite siblings (no Hold) carry
	// their inspector projection here so the coalesced renderer can
	// fold them into the prompt with full audit detail.
	auditByTU := make(map[string]conversation.AuditEvent)
	if auditBuf != nil {
		for _, ev := range auditBuf.entries {
			auditByTU[ev.ToolUse.ID] = ev
		}
	}
	for _, tu := range orderedToolUses {
		kind := holdKindFromVerdict(verdictByTU, tu.ID)
		c := pipeline.HoldCapture{
			ToolUse:   tu,
			ToolUseID: tu.ID,
			Kind:      kind,
		}
		if h, ok := holdByTU[tu.ID]; ok {
			c.ApprovalID = h.Pending.ID
			c.Stage = string(h.Pending.Stage)
			c.Payload = h.Pending
			c.InspectorSnapshot = llmproxy.InspectorSnapshot(h.Pending.Inspector)
		} else if ev, ok := auditByTU[tu.ID]; ok {
			c.InspectorSnapshot = ev.InspectorVerdict
		}
		finalizer.AddCapture(c)
	}
	if auditBuf != nil {
		for _, ev := range auditBuf.entries {
			finalizer.AddAudit(ev)
		}
	}
}

func holdKindFromVerdict(
	verdictByTU map[string]conversation.ToolUseVerdict,
	tuID string,
) conversation.HeldKindHint {
	if v, ok := verdictByTU[tuID]; ok {
		return pipeline.ClassifyVerdict(v)
	}
	return conversation.HeldKindHintDeny
}
