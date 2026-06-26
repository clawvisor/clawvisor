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
		// Inline (proxy-mediated) task creation defaults to sliding so
		// long-running agent workflows don't dead-end on a fixed
		// expiry. Direct /api/tasks callers still default to session;
		// see tasks.go.
		lifetime = "sliding"
	}
	if lifetime != "session" && lifetime != "standing" && lifetime != "sliding" {
		return nil, fmt.Errorf("invalid lifetime %q (want session, sliding, or standing)", req.Lifetime)
	}

	if lifetime == "standing" && req.ExpiresInSeconds > 0 {
		return nil, errors.New("expires_in_seconds cannot be set on a standing task")
	}
	expiresIn := req.ExpiresInSeconds
	if lifetime == "standing" {
		expiresIn = 0
	} else if expiresIn <= 0 {
		expiresIn = h.cfg.Task.DefaultExpirySeconds
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
	toolsRaw, egressRaw, credsRaw, err := runtimetasks.EnvelopeToRawColumns(runtimetasks.Envelope{
		ExpectedTools:       req.ExpectedTools,
		ExpectedEgress:      req.ExpectedEgress,
		RequiredCredentials: req.RequiredCredentials,
	})
	if err != nil {
		return nil, fmt.Errorf("encode task envelope: %w", err)
	}
	task.ExpectedTools = toolsRaw
	task.ExpectedEgress = egressRaw
	task.RequiredCredentials = credsRaw
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
			finalAssessment = taskrisk.MergeAssessments(llmAssessment, envelopeAssessment)
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
		// Match createInlineApprovedTask: the proxy-mediated path
		// defaults to sliding.
		lifetime = "sliding"
	}
	if lifetime != "session" && lifetime != "standing" && lifetime != "sliding" {
		return "", fmt.Errorf("invalid lifetime %q (want session, sliding, or standing)", req.Lifetime)
	}

	if lifetime == "standing" && req.ExpiresInSeconds > 0 {
		return "", errors.New("expires_in_seconds cannot be set on a standing task")
	}
	expiresIn := req.ExpiresInSeconds
	if lifetime == "standing" {
		expiresIn = 0
	} else if expiresIn <= 0 {
		expiresIn = h.cfg.Task.DefaultExpirySeconds
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
	toolsRaw, egressRaw, credsRaw, err := runtimetasks.EnvelopeToRawColumns(runtimetasks.Envelope{
		ExpectedTools:       req.ExpectedTools,
		ExpectedEgress:      req.ExpectedEgress,
		RequiredCredentials: req.RequiredCredentials,
	})
	if err != nil {
		return "", fmt.Errorf("encode task envelope: %w", err)
	}
	task.ExpectedTools = toolsRaw
	task.ExpectedEgress = egressRaw
	task.RequiredCredentials = credsRaw
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
			finalAssessment = taskrisk.MergeAssessments(llmAssessment, envelopeAssessment)
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
		// Rollback to expired so we don't leave a pending task with no
		// audit anchor. This is an operational failure before the user
		// sees an approval prompt, not a user denial.
		h.logger.ErrorContext(ctx, "failed to create pending inline approval record; expiring task to preserve audit invariant",
			"task_id", task.ID, "err", err)
		// Bounded detached context: WithoutCancel alone would let a
		// stalled store backend hang the inbound request goroutine.
		rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		rollbackErr := h.st.UpdateTaskStatus(rollbackCtx, task.ID, "expired")
		cancel()
		if rollbackErr != nil {
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
	// Detect the "dashboard already terminated this row" case explicitly.
	// Deny via the dashboard is permitted on chat-bound pending rows so
	// users can dismiss zombie tasks; when the model's "approve" reply
	// races in afterwards we want a clean signal back to
	// resolveInlineTaskApproval so it can render an explanatory reply
	// instead of a generic "approve failed" creator error. Same for
	// expired (24h sweep) and revoked (manual repair).
	if task.ApprovalSource == "inline_chat" {
		switch task.Status {
		case "denied", "expired", "revoked":
			return nil, &llmproxy.ErrInlineTaskAlreadyTerminal{Status: task.Status}
		}
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

	expiresAt := taskApprovedExpiresAt(task)

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
		// Lost the CAS to a concurrent resolver: dashboard Deny,
		// the chat-bound expiry sweep, or eviction-driven Expire.
		// Re-fetch so we can surface the typed terminal error if
		// the row landed at a known terminal state — same UX as
		// the pre-CAS early check above so the chat reply
		// renders "the user dismissed elsewhere; ask for a fresh
		// request" instead of a generic creator failure that
		// invites the model to retry the same body. Fall back to
		// the generic error if the re-fetch itself fails or the
		// row landed somewhere unexpected.
		if reread, rereadErr := h.st.GetTask(ctx, taskID); rereadErr == nil && reread != nil {
			switch reread.Status {
			case "denied", "expired", "revoked":
				return nil, &llmproxy.ErrInlineTaskAlreadyTerminal{Status: reread.Status}
			}
		}
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
		// client disconnect doesn't strand an orphan active task,
		// but cap with 5s timeouts — WithoutCancel alone strips
		// the parent deadline too, so a stalled store backend
		// would otherwise block this goroutine indefinitely. Each
		// step gets its OWN 5s budget so a slow task rollback
		// can't starve the canonical resolve.
		taskRollbackCtx, taskCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		rollbackErr := h.st.UpdateTaskStatus(taskRollbackCtx, task.ID, "denied")
		taskCancel()
		if rollbackErr != nil {
			// Task rollback failed: the row is still "active". DO
			// NOT also resolve the canonical record to deny/denied
			// here — that would create an audit-trail inversion
			// (record claims user denied, task row is active). The
			// CRITICAL log below is the operator's cue to
			// investigate; the canonical record stays pending
			// until manual repair.
			h.logger.ErrorContext(ctx, "CRITICAL: post-CAS credential mint failed AND rollback failed; task is now orphaned active",
				"task_id", task.ID, "mint_err", err, "rollback_err", rollbackErr)
			return nil, fmt.Errorf("mint credential placeholders: %w", err)
		}
		// Rollback succeeded — hydrate the in-memory task so the
		// canonical resolve validator sees pending → denied, then
		// flip the canonical record under its own bounded context.
		// Without this resolve the pending canonical row would sit
		// forever (the chat-bound expiry sweep filters by
		// status='pending_approval', so a "denied" task is invisible
		// to it), violating the audit invariant that every
		// chat-bound task eventually has a terminal canonical
		// resolution.
		task.Status = "denied"
		canonicalCtx, canonicalCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		h.resolveCanonicalTaskApproval(canonicalCtx, task, "task_create", "deny", "denied")
		canonicalCancel()
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

// ExpireInlineTask transitions a pending inline-chat task to expired.
// Called when the llmproxy cache evicts an awaiting_task_approval
// hold under capacity pressure: the chat anchor is gone, so chat
// approve can no longer resolve the row. Without this the dashboard
// would keep showing the task as pending_approval with "reply in
// chat" guidance that can never succeed. Distinct from
// DenyInlineTask because the user didn't dismiss it — the system
// did, for operational reasons; the canonical record resolution
// reuses the same "deny"/"expired" shape the 24h sweeper uses.
// Idempotent on already-terminal rows.
func (h *TasksHandler) ExpireInlineTask(ctx context.Context, taskID, userID string) error {
	task, err := h.st.GetTask(ctx, taskID)
	if err != nil {
		return err
	}
	if task.UserID != userID {
		return errors.New("not your task")
	}
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
	won, err := h.st.UpdateTaskStatusFrom(ctx, taskID, "pending_approval", "expired")
	if err != nil {
		return err
	}
	if !won {
		// Lost CAS to a concurrent resolver (sweeper or user
		// reply landing during eviction). Terminal state was
		// reached; report success so the eviction caller doesn't
		// double-publish or double-log.
		return nil
	}
	h.resolveCanonicalTaskApproval(ctx, task, "task_create", "deny", "expired")
	if h.eventHub != nil {
		h.eventHub.Publish(userID, events.Event{Type: "tasks"})
	}
	return nil
}

// ExpireInlineApprovedTask rolls back an active task the auto-approve
// gate landed via CreateInlineApprovedTask but whose post-creation
// steps (pending-substitution registration) failed before the verdict
// could ship. Without this rollback the task would sit "active" with
// no model record of having created it — wasted scope and a misleading
// audit row.
//
// Transitions active → expired only. Already-terminal rows (expired,
// denied, revoked) are no-ops so the rollback can run idempotently
// alongside the 24h TTL sweeper. Refuses to expire pending_approval
// tasks — those have their own ExpireInlineTask path (the cache LRU
// eviction flow).
func (h *TasksHandler) ExpireInlineApprovedTask(ctx context.Context, taskID, userID string) error {
	task, err := h.st.GetTask(ctx, taskID)
	if err != nil {
		return err
	}
	if task.UserID != userID {
		return errors.New("not your task")
	}
	switch task.Status {
	case "expired", "denied", "revoked":
		return nil
	case "active":
		// fall through to the CAS below.
	default:
		return fmt.Errorf("task is not an inline-approved active task (status=%q)", task.Status)
	}
	won, err := h.st.UpdateTaskStatusFrom(ctx, taskID, "active", "expired")
	if err != nil {
		return err
	}
	if !won {
		// Lost CAS to a concurrent transition (revoke, sweeper). The
		// task is no longer active, so the rollback's goal is met.
		return nil
	}
	h.resolveCanonicalTaskApproval(ctx, task, "task_create", "deny", "expired")
	if h.eventHub != nil {
		h.eventHub.Publish(userID, events.Event{Type: "tasks"})
	}
	return nil
}

// ── Inline-chat scope expansion ───────────────────────────────────────────────

// CreatePendingInlineExpansion is the lite-proxy entry point for the
// expansion intercept. Runs the same validation / derived-action /
// credential gates as the public Expand handler and lands the pending
// state via SetTaskPendingExpansion (CAS on active/expired). The
// canonical approval record's Surface is "inline_chat" so the
// dashboard treats it like an inline-chat task: visible, but resolved
// in chat. Returns the parent task id on success so the caller can
// pin the cache hold to it.
//
// Side effects:
//   - SetTaskPendingExpansion lands the pending envelope.
//   - createCanonicalInlineExpansionApprovalRecord writes the
//     approval_records row with Kind="task_expand", Surface="inline_chat",
//     Status="pending".
//   - SSE 'tasks' event so the Tasks page reflects the new state.
//
// Explicitly NOT done here (the chat is the only surface for THIS hold):
//   - No Telegram / push notification — those would race the chat
//     anchor and risk approving on two surfaces.
func (h *TasksHandler) CreatePendingInlineExpansion(
	ctx context.Context,
	agent *store.Agent,
	taskID string,
	additions *runtimetasks.Envelope,
	reason string,
	precomputedRisk *taskrisk.RiskAssessment,
) (string, error) {
	if agent == nil {
		return "", errors.New("agent is required")
	}
	if strings.TrimSpace(taskID) == "" {
		return "", errors.New("task id is required")
	}
	if additions == nil {
		return "", errors.New("expansion additions are required")
	}
	if strings.TrimSpace(reason) == "" {
		return "", errors.New("expansion reason is required")
	}
	if len(reason) > 512 {
		return "", fmt.Errorf("reason exceeds 512 bytes")
	}
	if len(additions.ExpectedTools) == 0 && len(additions.ExpectedEgress) == 0 && len(additions.RequiredCredentials) == 0 {
		return "", errors.New("expansion must declare at least one tool / egress / credential entry")
	}
	if issues := runtimepolicy.ValidateTaskEnvelopeAdditions(*additions); len(issues) > 0 {
		var msgs []string
		for _, issue := range issues {
			msgs = append(msgs, issue.Field+": "+issue.Message)
		}
		return "", fmt.Errorf("expansion envelope invalid: %s", strings.Join(msgs, "; "))
	}

	task, err := h.st.GetTask(ctx, taskID)
	if err != nil {
		return "", err
	}
	if task.UserID != agent.UserID {
		return "", errors.New("not your task")
	}
	if task.AgentID != agent.ID {
		return "", errors.New("not your task")
	}
	if task.Status != "active" && task.Status != "expired" {
		return "", fmt.Errorf("task must be active or expired to expand (status=%q)", task.Status)
	}

	// Validate derived gateway scopes up front so an unusable scope
	// fails before the user sees an approval prompt. Mirrors the
	// public Expand handler's per-entry gate; isolating it as an error
	// here means the intercept can fall through to the dashboard
	// rewrite cleanly, with the same diagnostic the agent would have
	// received from a direct POST.
	// Mirror the public Expand handler's wildcard skip so the
	// inline and dashboard flows agree on what counts as
	// redundant: when the parent already has a same-service
	// wildcard, mergeAuthorizedActionsFromExpansion silently drops
	// the derivation, so validating it would reject a harmless
	// `why`-only refinement.
	wildcardCoveredServices := make(map[string]struct{})
	for _, a := range task.AuthorizedActions {
		if a.Action == "*" {
			wildcardCoveredServices[strings.ToLower(strings.TrimSpace(a.Service))] = struct{}{}
		}
	}
	for i, tool := range additions.ExpectedTools {
		service, action, isGatewayAction := parseToolNameAsServiceAction(tool.ToolName)
		if !isGatewayAction {
			continue
		}
		if _, covered := wildcardCoveredServices[strings.ToLower(strings.TrimSpace(service))]; covered {
			continue
		}
		field := fmt.Sprintf("expected_tools[%d]", i)
		if detail, _, ok := h.validateDerivedAuthorizedAction(ctx, agent.UserID, service, action, field); !ok {
			return "", errors.New(detail.Error)
		}
	}

	// Validate credentials (added + would-be-replaced). Mirrors Expand's
	// validation so a chat-side intercept rejects unusable credentials
	// before the user sees an approval prompt.
	parentEnv, err := runtimetasks.EnvelopeFromTask(task)
	if err != nil {
		return "", fmt.Errorf("load parent envelope: %w", err)
	}
	merge := runtimetasks.MergeEnvelopes(parentEnv, *additions)
	credsToValidate := append([]runtimetasks.RequiredCredential(nil), merge.AddedCredentials...)
	for _, r := range merge.ReplacedCredentials {
		credsToValidate = append(credsToValidate, r.New)
	}
	if len(credsToValidate) > 0 {
		if err := h.validateTaskRequiredCredentials(ctx, task, credsToValidate); err != nil {
			return "", err
		}
	}

	pending, err := runtimetasks.PendingFromAdditions(*additions, reason)
	if err != nil {
		return "", fmt.Errorf("encode expansion envelope: %w", err)
	}
	// Stash the intercept-side merged LLM+floor risk on the pending row
	// so a dashboard-side approve (race against the chat anchor) also
	// reuses the LLM verdict — without this only the chat-side approve
	// would benefit from the cached read.
	if precomputedRisk != nil && precomputedRisk.RiskLevel != "" {
		pending.RiskAssessment = taskrisk.MarshalAssessment(precomputedRisk)
	}
	// Mark chat-bound so the dashboard ExpandApprove handler rejects with
	// INLINE_CHAT_BOUND (the cache hold owns resolution) and the dashboard
	// TaskCard hides its Approve button. Mirrors ApprovalSource="inline_chat"
	// on inline task creation.
	pending.Surface = "inline_chat"

	won, err := h.st.SetTaskPendingExpansion(ctx, taskID, pending)
	if err != nil {
		return "", fmt.Errorf("set pending expansion: %w", err)
	}
	if !won {
		// CAS lost: the task left active/expired between our GetTask
		// and the CAS write. Surface as a 409-shaped error so the
		// intercept can fall through cleanly.
		return "", fmt.Errorf("task is no longer in a state that can be expanded; re-fetch and retry")
	}
	// Hydrate the in-memory shape so the canonical-record helpers see
	// the diff in the same form the dashboard renderer would.
	task.PendingExpansion = pending
	task.Status = "pending_scope_expansion"

	if err := h.createCanonicalInlineExpansionApprovalRecord(ctx, task); err != nil {
		// Rollback the pending row so we don't leave the task wedged
		// in pending_scope_expansion with no anchor. Pick Active /
		// Expired based on the task's actual deadline — SetTaskPendingExpansion
		// CAS-allowed expand from BOTH active and expired, so we
		// can't just restore to active (that would revive an already-
		// expired session task as a side effect of a transient
		// record-create failure).
		rollbackStatus := store.ResolveExpansionStatusActive
		if task.ExpiresAt != nil && time.Now().After(*task.ExpiresAt) {
			rollbackStatus = store.ResolveExpansionStatusExpired
		}
		h.logger.ErrorContext(ctx, "failed to create inline expansion approval record; rolling back pending",
			"task_id", task.ID, "rollback_status", rollbackStatus, "err", err)
		rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		rolled, rollbackErr := h.st.ResolveTaskPendingExpansion(rollbackCtx, taskID, rollbackStatus)
		cancel()
		// If the rollback CAS errored OR lost (rolled=false because
		// the row already left pending_scope_expansion through
		// another path), the task is wedged in pending_scope_expansion
		// with no chat anchor that can resolve it. The pending_expansion_json
		// invariant is also at risk. Surface as CRITICAL so operators
		// see a wedged-task alert — the chat-bound expiry sweep will
		// eventually clear it, but that's the 24h fallback, not the
		// happy path.
		if rollbackErr != nil {
			h.logger.ErrorContext(ctx, "CRITICAL: inline expansion record-create failed AND rollback errored; task wedged in pending_scope_expansion",
				"task_id", task.ID, "record_err", err, "rollback_err", rollbackErr)
		} else if !rolled {
			h.logger.ErrorContext(ctx, "CRITICAL: inline expansion record-create failed AND rollback lost CAS; task may be wedged",
				"task_id", task.ID, "record_err", err)
		}
		return "", fmt.Errorf("create inline expansion approval record: %w", err)
	}

	if h.eventHub != nil {
		h.eventHub.Publish(agent.UserID, events.Event{Type: "tasks"})
	}
	return taskID, nil
}

// ApproveInlineExpansion is the chat-side approve path for an
// inline-chat scope expansion. Flips the pending_scope_expansion row
// through UpdateTaskEnvelopeFrom (same atomic merge + risk reassess +
// pending-snapshot guard as the public ExpandApprove handler), mints
// any new credential placeholders, and resolves the canonical
// approval record. Returns the InlineApprovedExpansion shape the
// rewrite path renders to the LLM.
//
// Thin wrapper that delegates to ApproveInlineExpansionWithAssessment
// with no precomputed assessment. Kept for backwards compatibility
// with the InlineExpansionCreator interface; callers that have a
// precomputed assessment from the inline intercept should prefer the
// WithAssessment variant so the LLM read is reused without
// re-running.
func (h *TasksHandler) ApproveInlineExpansion(ctx context.Context, taskID, userID string) (*llmproxy.InlineApprovedExpansion, error) {
	return h.ApproveInlineExpansionWithAssessment(ctx, taskID, userID, nil)
}

// ApproveInlineExpansionWithAssessment mirrors
// CreateInlineApprovedTaskWithAssessment on the creation side: it
// accepts the precomputed merged-envelope risk assessment from the
// inline-expansion intercept and threads it into reassessExpansionRisk
// so the persisted risk reflects the same LLM read the user saw in
// the chat prompt — without paying the LLM latency on the user's
// approve click.
func (h *TasksHandler) ApproveInlineExpansionWithAssessment(ctx context.Context, taskID, userID string, precomputed *taskrisk.RiskAssessment) (*llmproxy.InlineApprovedExpansion, error) {
	task, err := h.st.GetTask(ctx, taskID)
	if err != nil {
		return nil, err
	}
	if task.UserID != userID {
		return nil, errors.New("not your task")
	}
	// Detect "already resolved on another surface" up front, same
	// shape as the inline-task path: dashboard / notifier could have
	// approved or denied the expansion before our chat reply landed.
	if task.Status != "pending_scope_expansion" || task.PendingExpansion == nil {
		return nil, &llmproxy.ErrInlineExpansionAlreadyTerminal{Status: task.Status}
	}

	envUpdate, merged, err := buildExpansionApprovalUpdate(task)
	if err != nil {
		return nil, fmt.Errorf("build expansion update: %w", err)
	}
	// Prefer the assessment the inline intercept passed through the
	// pending hold — it was computed against the freshest envelope at
	// chat-prompt-render time. Fall back to whatever the request-side
	// Expand stashed on PendingTaskExpansion so dashboard-side approves
	// of inline-pending rows still get the LLM verdict; finally fall
	// back to deterministic-only for legacy rows.
	risk := precomputed
	if risk == nil {
		risk = decodePendingRiskAssessment(task.PendingExpansion)
	}
	reassessExpansionRisk(task, merged, &envUpdate, risk)
	// See ExpandApprove for the snapshot CAS rationale. Marshal
	// failure fails closed — silently skipping the guard would
	// disable stale-approve protection on the same approval that
	// surfaced the marshal bug.
	pendingJSON, mErr := json.Marshal(task.PendingExpansion)
	if mErr != nil {
		return nil, fmt.Errorf("snapshot pending expansion: %w", mErr)
	}
	envUpdate.ExpectedPendingJSON = pendingJSON
	expiresAt := taskApprovedExpiresAt(task)

	won, err := h.st.UpdateTaskEnvelopeFrom(ctx, taskID, "pending_scope_expansion", envUpdate, expiresAt)
	if err != nil {
		return nil, fmt.Errorf("apply expansion: %w", err)
	}
	if !won {
		// Re-fetch to surface the terminal state if the row landed at
		// a known terminal status — matches the inline-task ApproveInlineTask
		// re-fetch idiom so the chat reply can be specific.
		if reread, rereadErr := h.st.GetTask(ctx, taskID); rereadErr == nil && reread != nil {
			switch reread.Status {
			case "active", "denied", "expired", "revoked":
				return nil, &llmproxy.ErrInlineExpansionAlreadyTerminal{Status: reread.Status}
			}
		}
		return nil, errors.New("expansion was resolved by another caller")
	}
	task.Status = "active"
	task.ExpiresAt = &expiresAt
	// Hydrate the in-memory task's RequiredCredentials from the
	// MERGED envelope we just persisted — without this,
	// taskRequiredCredentials(task) would return the parent's
	// pre-expansion credential set and newly-added credentials
	// would never get placeholders minted (ensureTaskCredentialPlaceholders
	// is idempotent on already-minted items; missing items are
	// silently dropped). envUpdate.RequiredCredentials is the
	// post-merge JSON written to the row above.
	task.RequiredCredentials = envUpdate.RequiredCredentials

	// Mint credential placeholders for the merged credential set so
	// any newly-added entries get placeholders alongside the
	// already-minted ones. ensureTaskCredentialPlaceholders is
	// idempotent on already-minted vault items.
	requiredCredentials, credParseErr := taskRequiredCredentials(task)
	if credParseErr != nil {
		// Don't roll the envelope back — the user already approved
		// the expansion and the row is at status='active'. Surface
		// the parse failure loudly so operators see it; the chat
		// reply will omit placeholders for credentials we couldn't
		// decode, mirroring the post-CAS mint-failure path below.
		h.logger.ErrorContext(ctx, "post-CAS merged credentials parse failed; chat reply will omit placeholders",
			"task_id", task.ID, "err", credParseErr)
	}
	var placeholders []*store.RuntimePlaceholder
	credentialMintFailed := false
	if len(requiredCredentials) > 0 {
		var ensureErr error
		placeholders, ensureErr = h.ensureTaskCredentialPlaceholders(ctx, task, requiredCredentials, expiresAt)
		if ensureErr != nil {
			// Placeholder mint failed after the CAS landed. Distinct from
			// the inline-task path: we don't roll the envelope back here
			// because the user has already approved scope expansion that
			// is structurally valid; the credential mint failure is a
			// follow-up operational issue, not a permission decision. Log
			// loudly AND surface to the model via CredentialMintFailed so
			// the chat-reply augmentation can tell the model "scope
			// landed but creds didn't mint; ask the user to retry" rather
			// than silently omitting placeholders the model expects.
			h.logger.ErrorContext(ctx, "post-CAS expansion credential mint failed; surfacing to model via CredentialMintFailed",
				"task_id", task.ID, "err", ensureErr)
			credentialMintFailed = true
		}
	}
	// Also surface a parse failure as a mint failure to the model —
	// from the model's perspective both lead to the same outcome
	// (credentials it expected aren't usable).
	if credParseErr != nil {
		credentialMintFailed = true
	}

	rec, _ := h.findPendingTaskApprovalRecord(ctx, userID, taskID, "task_expand")
	h.resolveCanonicalTaskApproval(ctx, task, "task_expand", taskApprovalResolution(task), "approved")
	if h.eventHub != nil {
		h.eventHub.Publish(userID, events.Event{Type: "tasks"})
	}

	out := &llmproxy.InlineApprovedExpansion{
		TaskID:   task.ID,
		Status:   task.Status,
		Purpose:  task.Purpose,
		Lifetime: task.Lifetime,
	}
	if rec != nil {
		out.ApprovalRecordID = rec.ID
	}
	// Standing-task expansion: keep the response shape consistent
	// with CreateInlineApprovedTask for standing tasks — omit the
	// expires_at field rather than emitting the sentinel, so the
	// model doesn't read "expires 9999-01-01" as a real value.
	if task.Lifetime != "standing" && task.ExpiresAt != nil {
		out.ExpiresAtRFC3339 = task.ExpiresAt.Format(time.RFC3339)
	}
	out.Credentials = inlineCredentialPlaceholders(placeholders)
	out.CredentialMintFailed = credentialMintFailed
	return out, nil
}

// DenyInlineExpansion is the chat-side deny path. Routes through
// ResolveTaskPendingExpansion (clears pending_expansion_json AND sets
// status atomically). The task returns to active or expired —
// denying the EXPANSION, not the parent task, matching the public
// ExpandDeny handler's semantics. Idempotent on already-resolved rows.
func (h *TasksHandler) DenyInlineExpansion(ctx context.Context, taskID, userID string) error {
	task, err := h.st.GetTask(ctx, taskID)
	if err != nil {
		return err
	}
	if task.UserID != userID {
		return errors.New("not your task")
	}
	if task.Status != "pending_scope_expansion" {
		// Already resolved — treat as a success no-op. Same reasoning
		// as DenyInlineTask: the user's intent matches the terminal
		// state.
		return nil
	}

	newStatus := store.ResolveExpansionStatusActive
	if task.ExpiresAt != nil && time.Now().After(*task.ExpiresAt) {
		newStatus = store.ResolveExpansionStatusExpired
	}
	won, err := h.st.ResolveTaskPendingExpansion(ctx, taskID, newStatus)
	if err != nil {
		return err
	}
	if !won {
		// Lost CAS to another resolver (sweep, dashboard, notifier).
		// Terminal state was reached; report success so the chat-side
		// caller doesn't double-publish.
		return nil
	}
	h.resolveCanonicalTaskApproval(ctx, task, "task_expand", "deny", "denied")
	if h.eventHub != nil {
		h.eventHub.Publish(userID, events.Event{Type: "tasks"})
	}
	return nil
}

// ExpireInlineExpansion is called when the LRU cache evicts an
// inline-expansion hold under capacity pressure, OR as a rollback
// path when the intercept's Hold call fails after the pending row
// landed. Despite the name (kept for symmetry with ExpireInlineTask
// on the InlineTaskPendingCreator interface), this method does NOT
// unconditionally mark the parent task expired — that would terminate
// a previously-active task because the cache dropped its anchor.
//
// Instead we mirror DenyInlineExpansion's pattern: pick Active /
// Expired based on the parent's actual ExpiresAt. The expansion
// attempt is dropped either way; the parent's prior status is
// preserved. Idempotent on already-resolved rows.
func (h *TasksHandler) ExpireInlineExpansion(ctx context.Context, taskID, userID string) error {
	task, err := h.st.GetTask(ctx, taskID)
	if err != nil {
		return err
	}
	if task.UserID != userID {
		return errors.New("not your task")
	}
	if task.Status != "pending_scope_expansion" {
		return nil
	}
	// Mirror DenyInlineExpansion: the EXPANSION is what's dropped,
	// not the parent task. A still-alive parent (ExpiresAt > now)
	// must return to 'active', not be expired by the cache eviction.
	newStatus := store.ResolveExpansionStatusActive
	if task.ExpiresAt != nil && time.Now().After(*task.ExpiresAt) {
		newStatus = store.ResolveExpansionStatusExpired
	}
	won, err := h.st.ResolveTaskPendingExpansion(ctx, taskID, newStatus)
	if err != nil {
		return err
	}
	if !won {
		return nil
	}
	h.resolveCanonicalTaskApproval(ctx, task, "task_expand", "deny", string(newStatus))
	if h.eventHub != nil {
		h.eventHub.Publish(userID, events.Event{Type: "tasks"})
	}
	return nil
}

// createCanonicalInlineExpansionApprovalRecord writes the canonical
// approval_records row anchoring a chat-bound pending scope expansion.
// Surface is "inline_chat" (vs. the dashboard path's "dashboard") so
// dashboards filtering by surface see the inline-bound expansion
// distinctly. Mirrors createCanonicalPendingInlineApprovalRecord for
// task creation.
func (h *TasksHandler) createCanonicalInlineExpansionApprovalRecord(ctx context.Context, task *store.Task) error {
	payload, err := json.Marshal(task)
	if err != nil {
		return err
	}
	summary := map[string]any{
		"purpose":    task.Purpose,
		"lifetime":   task.Lifetime,
		"risk_level": task.RiskLevel,
	}
	if task.PendingExpansion != nil {
		additions, decErr := pendingExpansionToEnvelope(task.PendingExpansion)
		if decErr != nil {
			summary["decode_error"] = decErr.Error()
		} else {
			if len(additions.ExpectedTools) > 0 {
				summary["expected_tools"] = additions.ExpectedTools
			}
			if len(additions.ExpectedEgress) > 0 {
				summary["expected_egress"] = additions.ExpectedEgress
			}
			if len(additions.RequiredCredentials) > 0 {
				summary["required_credentials"] = additions.RequiredCredentials
			}
		}
		summary["reason"] = task.PendingExpansion.Reason
	}
	summaryJSON, err := json.Marshal(summary)
	if err != nil {
		return err
	}
	rec := &store.ApprovalRecord{
		ID:                  uuid.New().String(),
		Kind:                "task_expand",
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
