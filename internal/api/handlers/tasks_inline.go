package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/clawvisor/clawvisor/internal/events"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy"
	runtimepolicy "github.com/clawvisor/clawvisor/internal/runtime/policy"
	runtimetasks "github.com/clawvisor/clawvisor/internal/runtime/tasks"
	"github.com/clawvisor/clawvisor/internal/taskrisk"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// CreateInlineApprovedTask is the lite-proxy entry point invoked from
// the inline task-approval release path. The agent's "approve" reply on
// an awaiting_task_approval hold causes the lite-proxy to call this; it
// must atomically create the task in status=active and persist a
// canonical approval_records row with surface="inline_chat" so the
// audit trail matches what the dashboard surface produces (just
// resolved at creation time instead of after a queue trip).
//
// Side effects:
//   - Creates a store.Task with Status="active", ApprovalSource="inline_chat".
//   - Creates an ApprovalRecord with Kind="task_create",
//     Surface="inline_chat", Status="approved",
//     Resolution="allow_session"/"allow_always", ResolvedAt=now.
//   - Publishes SSE 'tasks' event so dashboards refresh.
//
// Explicitly skipped (vs. dashboard path):
//   - Telegram notifier — user is at the terminal, not asynchronous.
//   - 'queue' SSE event — the task never sat in the approval queue.
//   - Dedup cache — inline tasks are user-driven, not retry-prone.
//
// Returns an InlineApprovedTask shaped for the synthetic response
// surfaced back to the LLM via the lite-proxy's release path.
func (h *TasksHandler) CreateInlineApprovedTask(ctx context.Context, agent *store.Agent, req *runtimetasks.TaskCreateRequest, originalToolUseID string) (*llmproxy.InlineApprovedTask, error) {
	return h.createInlineApprovedTask(ctx, agent, req, originalToolUseID, nil)
}

// CreateInlineApprovedTaskWithAssessment is the auto-approve gate's
// fast-path entry. When the lite-proxy has already run the LLM risk
// assessor for the gate's intent-match check, it passes the resulting
// assessment here so we don't pay a second LLM round-trip — and the
// persisted task.RiskLevel is byte-identical to the level that
// justified bypassing the prompt. Passing nil (or an "unknown"
// assessment) falls back to computing fresh, matching the dashboard
// path's behavior.
func (h *TasksHandler) CreateInlineApprovedTaskWithAssessment(ctx context.Context, agent *store.Agent, req *runtimetasks.TaskCreateRequest, originalToolUseID string, precomputed *taskrisk.RiskAssessment) (*llmproxy.InlineApprovedTask, error) {
	return h.createInlineApprovedTask(ctx, agent, req, originalToolUseID, precomputed)
}

func (h *TasksHandler) createInlineApprovedTask(ctx context.Context, agent *store.Agent, req *runtimetasks.TaskCreateRequest, originalToolUseID string, precomputed *taskrisk.RiskAssessment) (*llmproxy.InlineApprovedTask, error) {
	if agent == nil {
		return nil, errors.New("agent is required")
	}
	if req == nil {
		return nil, errors.New("task request is required")
	}
	if strings.TrimSpace(req.Purpose) == "" {
		return nil, errors.New("task purpose is required")
	}

	hasRuntimeEnvelope := len(req.ExpectedTools) > 0 || len(req.ExpectedEgress) > 0
	if !hasRuntimeEnvelope {
		// Inline-approved tasks are exclusively driven by the lite-proxy's
		// model prompt which uses expected_tools. Reject empty
		// envelopes rather than silently accepting a scopeless task.
		return nil, errors.New("inline task must declare expected_tools or expected_egress")
	}

	env := runtimetasks.Envelope{
		ExpectedTools:          req.ExpectedTools,
		ExpectedEgress:         req.ExpectedEgress,
		RequiredCredentials:    req.RequiredCredentials,
		IntentVerificationMode: req.IntentVerificationMode,
		ExpectedUse:            req.ExpectedUse,
		SchemaVersion:          req.SchemaVersion,
	}
	if env.SchemaVersion == 0 {
		env.SchemaVersion = 2
	}
	if env.IntentVerificationMode == "" {
		env.IntentVerificationMode = "strict"
	}
	if issues := runtimepolicy.ValidateTaskEnvelope(env); len(issues) > 0 {
		var msgs []string
		for _, issue := range issues {
			msgs = append(msgs, issue.Field+": "+issue.Message)
		}
		return nil, fmt.Errorf("task envelope invalid: %s", strings.Join(msgs, "; "))
	}

	lifetime := req.Lifetime
	if lifetime == "" {
		lifetime = "session"
	}
	if lifetime != "session" && lifetime != "standing" {
		return nil, fmt.Errorf("invalid lifetime %q (want session or standing)", req.Lifetime)
	}

	expiresIn := req.ExpiresInSeconds
	if expiresIn <= 0 {
		expiresIn = h.cfg.Task.DefaultExpirySeconds
	}
	if lifetime == "standing" && req.ExpiresInSeconds > 0 {
		return nil, errors.New("expires_in_seconds cannot be set on a standing task")
	}
	requiredCredentials := req.RequiredCredentials

	// createInlineApprovedTask is only invoked from the release path
	// (resolveInlineTaskApproval, after the user's "approve" gesture)
	// or from the auto-approve gate (which constitutes approval — no
	// user gesture, but creation is the approval). In both cases
	// "now" is approval time, not hold time. Name it accordingly so
	// the scope-lifetime computation below is unambiguous.
	approvedAt := time.Now().UTC()
	task := &store.Task{
		ID:                     uuid.New().String(),
		UserID:                 agent.UserID,
		AgentID:                agent.ID,
		Purpose:                req.Purpose,
		Status:                 "active",
		Lifetime:               lifetime,
		IntentVerificationMode: env.IntentVerificationMode,
		ExpectedUse:            req.ExpectedUse,
		SchemaVersion:          env.SchemaVersion,
		ExpiresInSeconds:       expiresIn,
		ApprovalSource:         "inline_chat",
		ApprovedAt:             &approvedAt,
	}
	if lifetime != "standing" {
		// Task scope lifetime once approved. expires_in_seconds is
		// "usable scope after the user approves," not "time to
		// decide" — the awaiting_task_approval hold (see
		// inlineTaskApprovalHoldTTL) owns the decide window. So
		// regardless of how long the approval took to land, the
		// caller gets a full expiresIn of usable scope starting
		// now. The most common runtime case is expiresIn falling
		// back to task.default_expiry_seconds (config default
		// 1800 → 30 minutes of post-approval scope); callers
		// passing an explicit expires_in_seconds get exactly what
		// they asked for.
		expiresAt := approvedAt.Add(time.Duration(expiresIn) * time.Second)
		task.ExpiresAt = &expiresAt
	}
	if len(req.ExpectedTools) > 0 {
		raw, err := json.Marshal(req.ExpectedTools)
		if err != nil {
			return nil, fmt.Errorf("encode expected_tools: %w", err)
		}
		task.ExpectedTools = json.RawMessage(raw)
	}
	if len(req.ExpectedEgress) > 0 {
		raw, err := json.Marshal(req.ExpectedEgress)
		if err != nil {
			return nil, fmt.Errorf("encode expected_egress: %w", err)
		}
		task.ExpectedEgress = json.RawMessage(raw)
	}
	if len(req.RequiredCredentials) > 0 {
		raw, err := json.Marshal(req.RequiredCredentials)
		if err != nil {
			return nil, fmt.Errorf("encode required_credentials: %w", err)
		}
		task.RequiredCredentials = json.RawMessage(raw)
	}
	if err := h.validateTaskRequiredCredentials(ctx, task, requiredCredentials); err != nil {
		return nil, err
	}

	// Inline-approval rationale captures the gesture so a future audit
	// can see "the user approved this task at the chat terminal" without
	// joining tables.
	if originalToolUseID != "" {
		rationale, _ := json.Marshal(map[string]any{
			"surface":              "inline_chat",
			"original_approval_id": originalToolUseID,
		})
		task.ApprovalRationale = rationale
	}

	// Run the LLM-backed risk assessor and merge with the deterministic
	// envelope-shape policy for parity with the dashboard path. Failures
	// in either are non-fatal — a task should still be created with at
	// least the structural assessment when the LLM call errors out.
	//
	// Precomputed-assessment fast path: the auto-approve gate already
	// ran the assessor (with RecentUserTurns) before deciding to skip
	// the human prompt. Reusing its verdict here avoids a second
	// round-trip AND avoids the displayed task.RiskLevel disagreeing
	// with the level that justified the bypass. A nil or "unknown"
	// precomputed value falls through to the normal compute path —
	// the manual approval surface uses that branch and we keep its
	// behavior unchanged.
	envelopeAssessment := runtimepolicy.AssessTaskEnvelope(req.Purpose, env)
	finalAssessment := envelopeAssessment
	// Honor the precomputed value only when it carries a usable
	// risk level. nil, empty, and the literal "unknown" all fall
	// through to a fresh assessor call so we never persist a task
	// with an empty risk_level when the precomputed slot was set
	// but unpopulated.
	precomputedRisk := ""
	if precomputed != nil {
		precomputedRisk = strings.ToLower(strings.TrimSpace(precomputed.RiskLevel))
	}
	usePrecomputed := precomputed != nil && precomputedRisk != "" && precomputedRisk != "unknown"
	if usePrecomputed {
		finalAssessment = precomputed
	} else if h.assessor != nil {
		llmAssessment, err := h.assessor.Assess(ctx, taskrisk.AssessRequest{
			Purpose:                req.Purpose,
			AgentName:              agent.Name,
			UserID:                 agent.UserID,
			ExpectedTools:          env.ExpectedTools,
			ExpectedEgress:         env.ExpectedEgress,
			RequiredCredentials:    env.RequiredCredentials,
			IntentVerificationMode: env.IntentVerificationMode,
			ExpectedUse:            env.ExpectedUse,
		})
		if err != nil {
			h.logger.WarnContext(ctx, "inline task risk assessment failed", "error", err)
		}
		if llmAssessment != nil && !strings.EqualFold(llmAssessment.RiskLevel, "unknown") {
			finalAssessment = mergeRiskAssessments(llmAssessment, envelopeAssessment)
		}
	}
	if finalAssessment != nil {
		task.RiskLevel = finalAssessment.RiskLevel
		task.RiskDetails = taskrisk.MarshalAssessment(finalAssessment)
	}

	if err := h.st.CreateTask(ctx, task); err != nil {
		return nil, fmt.Errorf("create task: %w", err)
	}
	var credentialPlaceholders []*store.RuntimePlaceholder
	if len(requiredCredentials) > 0 {
		credentialExpiresAt := time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
		if task.ExpiresAt != nil {
			credentialExpiresAt = *task.ExpiresAt
		}
		var err error
		credentialPlaceholders, err = h.mintTaskCredentialPlaceholders(ctx, task, requiredCredentials, credentialExpiresAt)
		if err != nil {
			h.logger.ErrorContext(ctx, "failed to mint inline task credential placeholders; denying task to avoid orphaned active credential task",
				"task_id", task.ID, "err", err)
			// Rollback must outlive the inbound request — a client
			// disconnect that cancels ctx between the mint failure and
			// the status update would leave an orphaned active task
			// with no credentials. Detach the cancellation but inherit
			// values (logging, tracing).
			rollbackCtx := context.WithoutCancel(ctx)
			if rollbackErr := h.st.UpdateTaskStatus(rollbackCtx, task.ID, "denied"); rollbackErr != nil {
				h.logger.ErrorContext(ctx, "CRITICAL: credential placeholder mint failed AND rollback failed; task is now orphaned active",
					"task_id", task.ID, "mint_err", err, "rollback_err", rollbackErr)
			}
			return nil, fmt.Errorf("mint credential placeholders: %w", err)
		}
	}

	// Persist the canonical approval record at creation time. Surface
	// is "inline_chat" so dashboards filtering by surface see the
	// inline-approved tasks distinctly; resolution reflects the lifetime
	// (allow_session for session, allow_always for standing) to match
	// what taskApprovalResolution returns for the dashboard path.
	resolution := taskApprovalResolution(task)
	rec, err := h.createCanonicalInlineApprovalRecord(ctx, task, resolution, approvedAt)
	if err != nil {
		// Audit invariant: every active inline_chat task must have a
		// matching approval_records row. Without that row, we'd leave
		// a usable pre-approved task that no SOC/compliance trail can
		// account for. Roll the task back to status=denied so it can't
		// authorize anything, then fail the inline-create — the caller
		// will rewrite the user message as a deny with the approval
		// error surfaced to the LLM.
		h.logger.ErrorContext(ctx, "failed to create inline approval record; denying task to preserve audit invariant",
			"task_id", task.ID, "err", err)
		rollbackCtx := context.WithoutCancel(ctx)
		if rollbackErr := h.st.UpdateTaskStatus(rollbackCtx, task.ID, "denied"); rollbackErr != nil {
			// Best-effort: log loudly. The original error is what we
			// surface; an orphaned active task here is far worse than
			// any other failure mode, so flag it.
			h.logger.ErrorContext(ctx, "CRITICAL: approval record failed AND rollback failed; task is now orphaned active",
				"task_id", task.ID, "approval_err", err, "rollback_err", rollbackErr)
		}
		return nil, fmt.Errorf("create inline approval record: %w", err)
	}

	// SSE 'tasks' event so the dashboard reflects the new task. We
	// explicitly skip the 'queue' event because the task never sat in
	// the approval queue — emitting it would mislead a dashboard reader
	// into thinking something queued and was resolved.
	if h.eventHub != nil {
		h.eventHub.Publish(agent.UserID, events.Event{Type: "tasks"})
	}

	out := &llmproxy.InlineApprovedTask{
		ID:             task.ID,
		Status:         task.Status,
		Purpose:        task.Purpose,
		Lifetime:       task.Lifetime,
		ApprovalSource: task.ApprovalSource,
	}
	if rec != nil {
		out.ApprovalRecordID = rec.ID
	}
	if task.ExpiresAt != nil {
		out.ExpiresAtRFC3339 = task.ExpiresAt.Format(time.RFC3339)
	}
	out.Credentials = inlineCredentialPlaceholders(credentialPlaceholders)
	return out, nil
}

func inlineCredentialPlaceholders(placeholders []*store.RuntimePlaceholder) []llmproxy.InlineTaskCredentialPlaceholder {
	if len(placeholders) == 0 {
		return nil
	}
	out := make([]llmproxy.InlineTaskCredentialPlaceholder, 0, len(placeholders))
	for _, ph := range placeholders {
		if ph == nil || strings.TrimSpace(ph.Placeholder) == "" {
			continue
		}
		item := llmproxy.InlineTaskCredentialPlaceholder{
			VaultItemID:       ph.VaultItemID,
			ServiceID:         ph.ServiceID,
			Placeholder:       ph.Placeholder,
			CredentialGrantID: ph.CredentialGrantID,
		}
		if ph.ExpiresAt != nil {
			item.ExpiresAtRFC3339 = ph.ExpiresAt.Format(time.RFC3339)
		}
		out = append(out, item)
	}
	return out
}

// CreatePendingInlineTask is the lite-proxy entry point invoked from
// the inline-task intercept when the auto-approve gate refuses. Unlike
// CreateInlineApprovedTask (which produces an already-active task on
// the user's "approve" reply), this creates the task in
// status="pending_approval" so the dashboard's Tasks page renders it
// alongside any other awaiting-decision task. The actual approval
// transition (status flip to active, credential placeholder mint,
// canonical approval-record resolution) happens later when the user
// replies "approve" in chat — via ApproveInlineTask below — or via
// the existing dashboard Approve handler if the chat-bound guard is
// ever lifted.
//
// ApprovalSource is set to "inline_chat" at pending-creation time so
// the dashboard surface guard (TasksHandler.Approve / Deny) can
// detect chat-bound rows and refuse with INLINE_CHAT_BOUND rather
// than silently flipping the row without notifying the cache (the
// model would never see the approval).
//
// Side effects:
//   - Creates a store.Task with Status="pending_approval",
//     ApprovalSource="inline_chat", RiskLevel/Details from the
//     precomputed assessment when usable (or freshly computed).
//   - Creates an ApprovalRecord with Kind="task_create",
//     Surface="inline_chat", Status="pending" (no Resolution / no
//     ResolvedAt yet — those land at approve time).
//   - Publishes SSE 'tasks' event so the Tasks page refreshes.
//
// Explicitly NOT done here (deferred to the approve transition):
//   - Mints credential placeholders.
//   - Resolves the canonical approval record.
//
// Returns the new task ID so the caller can hand it into the
// llmproxy cache hold (it lands in PendingLiteApproval.PendingTaskID).
func (h *TasksHandler) CreatePendingInlineTask(ctx context.Context, agent *store.Agent, req *runtimetasks.TaskCreateRequest, originalToolUseID string, precomputed *taskrisk.RiskAssessment) (string, error) {
	if agent == nil {
		return "", errors.New("agent is required")
	}
	if req == nil {
		return "", errors.New("task request is required")
	}
	if strings.TrimSpace(req.Purpose) == "" {
		return "", errors.New("task purpose is required")
	}

	hasRuntimeEnvelope := len(req.ExpectedTools) > 0 || len(req.ExpectedEgress) > 0
	if !hasRuntimeEnvelope {
		return "", errors.New("inline task must declare expected_tools or expected_egress")
	}

	env := runtimetasks.Envelope{
		ExpectedTools:          req.ExpectedTools,
		ExpectedEgress:         req.ExpectedEgress,
		RequiredCredentials:    req.RequiredCredentials,
		IntentVerificationMode: req.IntentVerificationMode,
		ExpectedUse:            req.ExpectedUse,
		SchemaVersion:          req.SchemaVersion,
	}
	if env.SchemaVersion == 0 {
		env.SchemaVersion = 2
	}
	if env.IntentVerificationMode == "" {
		env.IntentVerificationMode = "strict"
	}
	if issues := runtimepolicy.ValidateTaskEnvelope(env); len(issues) > 0 {
		var msgs []string
		for _, issue := range issues {
			msgs = append(msgs, issue.Field+": "+issue.Message)
		}
		return "", fmt.Errorf("task envelope invalid: %s", strings.Join(msgs, "; "))
	}

	lifetime := req.Lifetime
	if lifetime == "" {
		lifetime = "session"
	}
	if lifetime != "session" && lifetime != "standing" {
		return "", fmt.Errorf("invalid lifetime %q (want session or standing)", req.Lifetime)
	}

	expiresIn := req.ExpiresInSeconds
	if expiresIn <= 0 {
		expiresIn = h.cfg.Task.DefaultExpirySeconds
	}
	if lifetime == "standing" && req.ExpiresInSeconds > 0 {
		return "", errors.New("expires_in_seconds cannot be set on a standing task")
	}

	task := &store.Task{
		ID:                     uuid.New().String(),
		UserID:                 agent.UserID,
		AgentID:                agent.ID,
		Purpose:                req.Purpose,
		Status:                 "pending_approval",
		Lifetime:               lifetime,
		IntentVerificationMode: env.IntentVerificationMode,
		ExpectedUse:            req.ExpectedUse,
		SchemaVersion:          env.SchemaVersion,
		ExpiresInSeconds:       expiresIn,
		ApprovalSource:         "inline_chat",
	}
	if len(req.ExpectedTools) > 0 {
		raw, err := json.Marshal(req.ExpectedTools)
		if err != nil {
			return "", fmt.Errorf("encode expected_tools: %w", err)
		}
		task.ExpectedTools = json.RawMessage(raw)
	}
	if len(req.ExpectedEgress) > 0 {
		raw, err := json.Marshal(req.ExpectedEgress)
		if err != nil {
			return "", fmt.Errorf("encode expected_egress: %w", err)
		}
		task.ExpectedEgress = json.RawMessage(raw)
	}
	if len(req.RequiredCredentials) > 0 {
		raw, err := json.Marshal(req.RequiredCredentials)
		if err != nil {
			return "", fmt.Errorf("encode required_credentials: %w", err)
		}
		task.RequiredCredentials = json.RawMessage(raw)
	}
	// Validate credential availability up front so the user doesn't
	// see an approval prompt for a task that can't possibly authorize
	// — matches the dashboard Create flow's behavior. Placeholder
	// minting itself is deferred to the approve transition.
	if err := h.validateTaskRequiredCredentials(ctx, task, req.RequiredCredentials); err != nil {
		return "", err
	}

	// Stamp the original tool_use ID into ApprovalRationale so post-
	// approval audit can correlate the chat gesture without joining
	// across event tables.
	if originalToolUseID != "" {
		rationale, _ := json.Marshal(map[string]any{
			"surface":              "inline_chat",
			"original_approval_id": originalToolUseID,
		})
		task.ApprovalRationale = rationale
	}

	// Risk assessment: precomputed → use; otherwise compute. Same
	// precedence rules as createInlineApprovedTask so the displayed
	// RiskLevel is consistent across the two flows.
	envelopeAssessment := runtimepolicy.AssessTaskEnvelope(req.Purpose, env)
	finalAssessment := envelopeAssessment
	precomputedRisk := ""
	if precomputed != nil {
		precomputedRisk = strings.ToLower(strings.TrimSpace(precomputed.RiskLevel))
	}
	usePrecomputed := precomputed != nil && precomputedRisk != "" && precomputedRisk != "unknown"
	if usePrecomputed {
		finalAssessment = precomputed
	} else if h.assessor != nil {
		llmAssessment, err := h.assessor.Assess(ctx, taskrisk.AssessRequest{
			Purpose:                req.Purpose,
			AgentName:              agent.Name,
			UserID:                 agent.UserID,
			ExpectedTools:          env.ExpectedTools,
			ExpectedEgress:         env.ExpectedEgress,
			RequiredCredentials:    env.RequiredCredentials,
			IntentVerificationMode: env.IntentVerificationMode,
			ExpectedUse:            env.ExpectedUse,
		})
		if err != nil {
			h.logger.WarnContext(ctx, "inline pending task risk assessment failed", "error", err)
		}
		if llmAssessment != nil && !strings.EqualFold(llmAssessment.RiskLevel, "unknown") {
			finalAssessment = mergeRiskAssessments(llmAssessment, envelopeAssessment)
		}
	}
	if finalAssessment != nil {
		task.RiskLevel = finalAssessment.RiskLevel
		task.RiskDetails = taskrisk.MarshalAssessment(finalAssessment)
	}

	if err := h.st.CreateTask(ctx, task); err != nil {
		return "", fmt.Errorf("create pending inline task: %w", err)
	}

	// Canonical approval_records row, surface=inline_chat, status=
	// pending. Resolved at approve/deny time by the chat-side
	// resolveCanonicalTaskApproval call. Without this row the audit
	// trail couldn't account for "a chat-bound task sat pending."
	if err := h.createCanonicalPendingInlineApprovalRecord(ctx, task); err != nil {
		// Rollback to denied so we don't leave a pending task with no
		// audit anchor.
		h.logger.ErrorContext(ctx, "failed to create pending inline approval record; denying task to preserve audit invariant",
			"task_id", task.ID, "err", err)
		rollbackCtx := context.WithoutCancel(ctx)
		if rollbackErr := h.st.UpdateTaskStatus(rollbackCtx, task.ID, "denied"); rollbackErr != nil {
			h.logger.ErrorContext(ctx, "CRITICAL: pending approval record failed AND rollback failed; task is now orphaned pending",
				"task_id", task.ID, "approval_err", err, "rollback_err", rollbackErr)
		}
		return "", fmt.Errorf("create pending inline approval record: %w", err)
	}

	if h.eventHub != nil {
		h.eventHub.Publish(agent.UserID, events.Event{Type: "tasks"})
	}
	return task.ID, nil
}

// ApproveInlineTask flips a pending inline-chat task to active. Called
// from the lite-proxy chat resolution path when the user's reply is
// "approve". Bypasses the dashboard CHAT_APPROVAL_REQUIRED guard
// because, by definition, this caller IS the chat surface — the model
// is about to see the substituted approval reply. Returns the
// InlineApprovedTask shape the caller hands to the LLM.
func (h *TasksHandler) ApproveInlineTask(ctx context.Context, taskID, userID string) (*llmproxy.InlineApprovedTask, error) {
	task, err := h.st.GetTask(ctx, taskID)
	if err != nil {
		return nil, err
	}
	if task.UserID != userID {
		return nil, errors.New("not your task")
	}
	if task.Status != "pending_approval" || task.ApprovalSource != "inline_chat" {
		return nil, fmt.Errorf("task is not a pending inline-chat task (status=%q, source=%q)", task.Status, task.ApprovalSource)
	}

	requiredCredentials, err := taskRequiredCredentials(task)
	if err != nil {
		return nil, fmt.Errorf("could not parse required_credentials: %w", err)
	}
	if err := h.validateTaskRequiredCredentials(ctx, task, requiredCredentials); err != nil {
		return nil, err
	}

	var expiresAt time.Time
	if task.Lifetime == "standing" {
		expiresAt = time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
	} else {
		expiresAt = time.Now().UTC().Add(time.Duration(task.ExpiresInSeconds) * time.Second)
	}

	// CAS pending → active FIRST, then mint placeholders. The expiry
	// sweeper specifically targets approval_source='inline_chat'
	// pending rows past the 24h hold TTL — running mint before the
	// CAS would leave credential placeholders bound to a task the
	// sweeper has just denied, with no cleanup path. Doing the CAS
	// first means a lost race surfaces as "no longer pending"
	// before any side effects fire. The minor cost is one extra
	// rollback path when mint fails post-CAS, which we handle
	// explicitly below.
	won, err := h.st.UpdateTaskApprovedFrom(ctx, taskID, "pending_approval", expiresAt, task.AuthorizedActions)
	if err != nil {
		return nil, err
	}
	if !won {
		return nil, errors.New("task is no longer pending approval")
	}
	task.Status = "active"
	now := time.Now().UTC()
	task.ApprovedAt = &now
	task.ExpiresAt = &expiresAt

	placeholders, err := h.ensureTaskCredentialPlaceholders(ctx, task, requiredCredentials, expiresAt)
	if err != nil {
		// Mint failed AFTER the CAS landed. Roll the task back to
		// denied so we don't leave an active task with no usable
		// credentials. Detach the cancellation so a mid-request
		// client disconnect doesn't strand an orphan active task.
		rollbackCtx := context.WithoutCancel(ctx)
		if rollbackErr := h.st.UpdateTaskStatus(rollbackCtx, task.ID, "denied"); rollbackErr != nil {
			h.logger.ErrorContext(ctx, "CRITICAL: post-CAS credential mint failed AND rollback failed; task is now orphaned active",
				"task_id", task.ID, "mint_err", err, "rollback_err", rollbackErr)
		}
		return nil, fmt.Errorf("mint credential placeholders: %w", err)
	}

	// Snapshot the pending canonical record BEFORE resolve flips it;
	// findPendingTaskApprovalRecord filters to status="pending" so a
	// post-resolve lookup would return ErrNotFound and the
	// InlineApprovedTask response wouldn't carry the record id.
	rec, _ := h.findPendingTaskApprovalRecord(ctx, userID, taskID, "task_create")
	h.resolveCanonicalTaskApproval(ctx, task, "task_create", taskApprovalResolution(task), "approved")
	if h.eventHub != nil {
		h.eventHub.Publish(userID, events.Event{Type: "tasks"})
	}

	out := &llmproxy.InlineApprovedTask{
		ID:             task.ID,
		Status:         task.Status,
		Purpose:        task.Purpose,
		Lifetime:       task.Lifetime,
		ApprovalSource: task.ApprovalSource,
	}
	if rec != nil {
		out.ApprovalRecordID = rec.ID
	}
	if task.ExpiresAt != nil {
		out.ExpiresAtRFC3339 = task.ExpiresAt.Format(time.RFC3339)
	}
	out.Credentials = inlineCredentialPlaceholders(placeholders)
	return out, nil
}

// DenyInlineTask transitions a pending inline-chat task to denied.
// Symmetric to ApproveInlineTask: bypasses the dashboard guard
// because the chat surface is doing the denial.
func (h *TasksHandler) DenyInlineTask(ctx context.Context, taskID, userID string) error {
	task, err := h.st.GetTask(ctx, taskID)
	if err != nil {
		return err
	}
	if task.UserID != userID {
		return errors.New("not your task")
	}
	// Idempotency: a task already in a terminal state matching the
	// user's intent (denied / expired) is a successful no-op rather
	// than an error. The common cause is the expiry sweeper having
	// already denied the row between our GetTask and the CAS — the
	// model still gets a "denied" reply, and we don't want to stuff
	// "task is no longer pending" into out.Reason and the SSE log.
	switch task.Status {
	case "denied", "expired", "revoked":
		return nil
	case "pending_approval":
		if task.ApprovalSource != "inline_chat" {
			return fmt.Errorf("task is not a pending inline-chat task (source=%q)", task.ApprovalSource)
		}
	default:
		return fmt.Errorf("task is not a pending inline-chat task (status=%q, source=%q)", task.Status, task.ApprovalSource)
	}
	won, err := h.st.UpdateTaskStatusFrom(ctx, taskID, "pending_approval", "denied")
	if err != nil {
		return err
	}
	if !won {
		// Lost the CAS to another resolver (sweeper, parallel deny).
		// The terminal state is what the user asked for, so report
		// success — the side effects below would double-fire
		// otherwise.
		return nil
	}
	h.resolveCanonicalTaskApproval(ctx, task, "task_create", "deny", "denied")
	if h.eventHub != nil {
		h.eventHub.Publish(userID, events.Event{Type: "tasks"})
	}
	return nil
}

// createCanonicalPendingInlineApprovalRecord writes the canonical
// approval_records row anchoring a chat-bound pending task. Status is
// "pending" with no Resolution/ResolvedAt — those land at chat-approve
// time via resolveCanonicalTaskApproval, matching the dashboard path's
// shape.
func (h *TasksHandler) createCanonicalPendingInlineApprovalRecord(ctx context.Context, task *store.Task) error {
	payload, err := json.Marshal(task)
	if err != nil {
		return err
	}
	summary := map[string]any{
		"purpose":    task.Purpose,
		"lifetime":   task.Lifetime,
		"risk_level": task.RiskLevel,
	}
	summaryJSON, err := json.Marshal(summary)
	if err != nil {
		return err
	}
	rec := &store.ApprovalRecord{
		ID:                  uuid.New().String(),
		Kind:                "task_create",
		UserID:              task.UserID,
		AgentID:             &task.AgentID,
		TaskID:              &task.ID,
		Status:              "pending",
		Surface:             "inline_chat",
		SummaryJSON:         json.RawMessage(summaryJSON),
		PayloadJSON:         json.RawMessage(payload),
		ResolutionTransport: "inline_chat",
	}
	return h.st.CreateApprovalRecord(ctx, rec)
}

// createCanonicalInlineApprovalRecord writes the approval_records row
// for an inline-approved task. Mirrors createCanonicalTaskApproval but
// resolves the row at creation time with surface=inline_chat and a
// non-empty Resolution. Returns the inserted record so callers can
// reference its id.
func (h *TasksHandler) createCanonicalInlineApprovalRecord(ctx context.Context, task *store.Task, resolution string, resolvedAt time.Time) (*store.ApprovalRecord, error) {
	payload, err := json.Marshal(task)
	if err != nil {
		return nil, err
	}
	summary := map[string]any{
		"purpose":    task.Purpose,
		"lifetime":   task.Lifetime,
		"risk_level": task.RiskLevel,
	}
	summaryJSON, err := json.Marshal(summary)
	if err != nil {
		return nil, err
	}
	rec := &store.ApprovalRecord{
		ID:                  uuid.New().String(),
		Kind:                "task_create",
		UserID:              task.UserID,
		AgentID:             &task.AgentID,
		TaskID:              &task.ID,
		Status:              "approved",
		Surface:             "inline_chat",
		SummaryJSON:         json.RawMessage(summaryJSON),
		PayloadJSON:         json.RawMessage(payload),
		ResolutionTransport: "inline_chat",
		Resolution:          resolution,
		ResolvedAt:          &resolvedAt,
	}
	if err := h.st.CreateApprovalRecord(ctx, rec); err != nil {
		return nil, err
	}
	return rec, nil
}
