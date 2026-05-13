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
		// model prompt which uses expected_tools_json. Reject empty
		// envelopes rather than silently accepting a scopeless task.
		return nil, errors.New("inline task must declare expected_tools_json or expected_egress_json")
	}

	env := runtimetasks.Envelope{
		ExpectedTools:          req.ExpectedTools,
		ExpectedEgress:         req.ExpectedEgress,
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

	now := time.Now().UTC()
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
		ApprovedAt:             &now,
	}
	if lifetime != "standing" {
		expiresAt := now.Add(time.Duration(expiresIn) * time.Second)
		task.ExpiresAt = &expiresAt
	}
	if len(req.ExpectedTools) > 0 {
		raw, err := json.Marshal(req.ExpectedTools)
		if err != nil {
			return nil, fmt.Errorf("encode expected_tools_json: %w", err)
		}
		task.ExpectedTools = json.RawMessage(raw)
	}
	if len(req.ExpectedEgress) > 0 {
		raw, err := json.Marshal(req.ExpectedEgress)
		if err != nil {
			return nil, fmt.Errorf("encode expected_egress_json: %w", err)
		}
		task.ExpectedEgress = json.RawMessage(raw)
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

	// Best-effort risk assessment for parity with the dashboard path; a
	// failure here is non-fatal (the dashboard path also swallows it).
	if h.assessor != nil {
		envelopeAssessment := runtimepolicy.AssessTaskEnvelope(req.Purpose, env)
		if envelopeAssessment != nil {
			task.RiskLevel = envelopeAssessment.RiskLevel
			task.RiskDetails = taskrisk.MarshalAssessment(envelopeAssessment)
		}
	}

	if err := h.st.CreateTask(ctx, task); err != nil {
		return nil, fmt.Errorf("create task: %w", err)
	}

	// Persist the canonical approval record at creation time. Surface
	// is "inline_chat" so dashboards filtering by surface see the
	// inline-approved tasks distinctly; resolution reflects the lifetime
	// (allow_session for session, allow_always for standing) to match
	// what taskApprovalResolution returns for the dashboard path.
	resolution := taskApprovalResolution(task)
	rec, err := h.createCanonicalInlineApprovalRecord(ctx, task, resolution, now)
	if err != nil {
		h.logger.Error("failed to create inline approval record", "task_id", task.ID, "err", err)
		// We still return the task — the task is real and active; the
		// missing approval_records row is a degraded audit signal but
		// shouldn't block the user-visible flow.
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
	return out, nil
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
