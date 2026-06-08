package llmproxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/inspector"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/pricing"
	"github.com/clawvisor/clawvisor/pkg/store"
	"github.com/clawvisor/clawvisor/pkg/version"
	"github.com/google/uuid"
)

// AuditEmitter wraps store.LogAudit with lite-proxy-shaped helpers. Each
// helper writes one row into audit_log. The shape conforms to the
// existing dashboard surface (Audit.tsx) so lite-proxy events show up
// alongside gateway events without UI changes.
//
// Forensic fields (validator prompt SHA, parser version, clawvisor build
// SHA) are stashed in ParamsSafe so an audit row is self-contained — a
// future incident reconstruction can identify exactly which inspector
// build produced the verdict.
type AuditEmitter struct {
	Store  store.Store
	Logger *slog.Logger

	// ValidatorPromptSHA is recorded on every tool_use audit row so a
	// prompt change is forensically visible. Set by the handler when it
	// knows the active validator's prompt hash.
	ValidatorPromptSHA string
}

// NewAuditEmitter builds an AuditEmitter with sensible defaults. Logger
// nil falls back to slog.Default(); pass an inspector.AnthropicValidator
// (or any type with a PromptSHA() method) to populate forensics.
func NewAuditEmitter(st store.Store, logger *slog.Logger, v interface{ PromptSHA() string }) *AuditEmitter {
	if logger == nil {
		logger = slog.Default()
	}
	e := &AuditEmitter{Store: st, Logger: logger}
	if v != nil {
		e.ValidatorPromptSHA = v.PromptSHA()
	}
	return e
}

// EndpointCallExtras carries optional, request-call-specific signals
// that LogEndpointCall threads into the audit row and (when present)
// the llm_request_cost row. Keeping these out of the positional args
// avoids breaking the many callers that don't supply them.
type EndpointCallExtras struct {
	// TaskID, when non-empty, populates llm_request_cost.task_id so
	// per-task spend rolls up. It deliberately does NOT propagate to
	// audit_log.task_id: that column is part of the canonical dedup
	// key UNIQUE(user_id, request_id, COALESCE(task_id, '')) and the
	// same request_id already has task-scoped audit rows landing
	// from LogToolUseInspected / LogInlineTaskApproved at
	// (uid, rid, T). Adding the endpoint_call row at the same key
	// would silently dedup it out — and the cost row with it.
	// Leaving audit_log.task_id NULL keeps the legacy dedup behavior
	// and the cost table carries the task linkage independently.
	TaskID string
	// Usage, when Found, drives one llm_request_cost row computed
	// against the pricing table. Skipped when nil or not Found.
	Usage *ExtractUsageResult
}

// LogEndpointCall records one /v1/* request hitting the lite-proxy LLM
// endpoint. Service is the provider name; Action is the route shape
// ("messages.create", "responses.create", "chat.completions.create").
// outcome is "success" / "error_<status>" / "upstream_key_missing" etc.
//
// When extras.Usage is non-nil and reports Found, the call also writes
// one llm_request_cost row tying the audit entry to its token + price
// breakdown. Cost-record failures are logged but never block the
// audit insert — billing is best-effort observability, not a gate.
func (e *AuditEmitter) LogEndpointCall(ctx context.Context, agent *store.Agent, requestID, provider, action string, statusCode int, decision, outcome, reason string, duration time.Duration, paramsExtra map[string]any, extras EndpointCallExtras) {
	if e == nil || e.Store == nil || agent == nil {
		return
	}
	params := map[string]any{
		"event":             "lite_proxy.endpoint_call",
		"http_status":       statusCode,
		"build_sha":         buildSHA(),
		"validator_prompt":  e.ValidatorPromptSHA,
		"parser_version":    parserVersion(),
		"clawvisor_version": version.Version,
	}
	for k, v := range paramsExtra {
		params[k] = v
	}
	paramsJSON, _ := json.Marshal(params)

	var taskIDPtr *string
	if extras.TaskID != "" {
		t := extras.TaskID
		taskIDPtr = &t
	}
	// audit_log.task_id is deliberately left NULL on endpoint_call
	// rows — see EndpointCallExtras.TaskID for the dedup rationale.
	// The task linkage is recorded on the cost row below.
	entry := &store.AuditEntry{
		ID:         uuid.NewString(),
		UserID:     agent.UserID,
		AgentID:    &agent.ID,
		RequestID:  requestID,
		Timestamp:  time.Now().UTC(),
		Service:    provider,
		Action:     action,
		ParamsSafe: paramsJSON,
		Decision:   decision,
		Outcome:    outcome,
		Reason:     nilIfEmpty(SafeAuditErrorDetail(reason)),
		DurationMS: int(duration.Milliseconds()),
	}
	if err := e.Store.LogAudit(ctx, entry); err != nil {
		// ErrConflict means the canonical audit row for this
		// (user_id, request_id, task_id) already exists — this is
		// the dedup path the schema is designed to enforce, not a
		// store failure. Fall through and still record cost so the
		// retried request's billable usage isn't permanently lost.
		//
		// BUT: llm_request_cost.audit_id has a FK into audit_log(id)
		// ON DELETE CASCADE, so using the locally generated entry.ID
		// (which never landed) would violate the FK. Resolve the
		// surviving canonical row via FindDedupCandidate — the audit
		// task_id used for dedup is COALESCE(task_id, ''), and
		// endpoint_call rows deliberately leave audit_log.task_id
		// NULL (see EndpointCallExtras.TaskID), so we look up with
		// taskID="" to match the canonical pre-task row.
		if errors.Is(err, store.ErrConflict) {
			canonical, lookupErr := e.Store.FindDedupCandidate(ctx, requestID, agent.UserID, "")
			if lookupErr != nil || canonical == nil {
				// Defensive: ErrConflict means the row exists, so
				// the lookup should succeed. If it doesn't, skip the
				// cost row rather than risk an FK violation — the
				// tokens are still observable via the warn below.
				e.Logger.WarnContext(ctx, "lite-proxy: audit deduped but canonical lookup failed; skipping cost record",
					"agent_id", agent.ID, "request_id", requestID, "action", action, "err", errString(lookupErr))
				return
			}
			entry.ID = canonical.ID
			entry.Timestamp = canonical.Timestamp
			e.Logger.DebugContext(ctx, "lite-proxy: audit log deduped; recording cost against canonical row",
				"agent_id", agent.ID, "request_id", requestID, "action", action, "canonical_audit_id", canonical.ID)
		} else {
			// Surface the token counts to slog on the failure path so
			// a dropped row leaves a trace that can be reconciled
			// later — without this, an audit-log outage silently
			// loses the per-request usage data entirely.
			var tokenAttrs []any
			tokenAttrs = append(tokenAttrs, "agent_id", agent.ID, "action", action, "err", err.Error())
			if extras.Usage != nil && extras.Usage.Found {
				tokenAttrs = append(tokenAttrs,
					"usage_model", extras.Usage.Model,
					"usage_input_tokens", extras.Usage.Usage.InputTokens,
					"usage_output_tokens", extras.Usage.Usage.OutputTokens,
					"usage_cache_read_tokens", extras.Usage.Usage.CacheReadTokens,
					"usage_cache_write_tokens", extras.Usage.Usage.CacheWriteTokens,
					"usage_cache_write_1h_tokens", extras.Usage.Usage.CacheWrite1hTokens,
				)
			}
			e.Logger.WarnContext(ctx, "lite-proxy: audit log failed", tokenAttrs...)
			// Skip cost recording on real store errors. The cost
			// row carries no FK to audit_log so technically we could
			// land it anyway, but a store error on audit suggests
			// the cost insert would likely fail too — and the
			// tokens above are in slog for reconciliation.
			return
		}
	}
	if extras.Usage != nil && extras.Usage.Found {
		// Store the normalized model id so GROUP BY in GetTaskCost
		// doesn't fragment a task's spend across spelling variants
		// (e.g. `claude-opus-4-7` and `anthropic/claude-opus-4-7` and
		// `claude-opus-4-7-20260120` are the same priced model).
		normModel := pricing.Normalize(extras.Usage.Model)
		if normModel == "" {
			// Both the upstream body and the inbound request omitted
			// a model id. The cost row would be unattributable (it
			// can't be priced, can't roll up under any model in the
			// UI). Skip the insert and warn — the token counts are
			// still in slog above for forensics.
			e.Logger.WarnContext(ctx, "lite-proxy: skipping cost record — no model on request or response",
				"agent_id", agent.ID, "audit_id", entry.ID)
		} else {
			cost := pricing.Compute(normModel, extras.Usage.Usage)
			var costMicros *int64
			if cost.Known {
				c := cost.CostMicros
				costMicros = &c
			} else {
				// Surface unknown-model rows so the pricing table can
				// be updated. Recorded with cost_micros=NULL so
				// aggregates can flag them rather than under-billing
				// silently.
				e.Logger.WarnContext(ctx, "lite-proxy: cost not priced — model missing from pricing table",
					"agent_id", agent.ID, "model", normModel)
			}
			// Storage flattens the per-TTL cache-write breakdown into
			// one column; cost has already been computed against the
			// split buckets by pricing.Compute above. Re-deriving cost
			// from the stored row would assume 5m rates and slightly
			// under-bill 1h cache writes — acceptable for re-derivation
			// today, callable out in a future schema bump if needed.
			cacheWriteTotal := extras.Usage.Usage.CacheWriteTokens + extras.Usage.Usage.CacheWrite1hTokens
			row := &store.LLMRequestCost{
				AuditID:          entry.ID,
				UserID:           agent.UserID,
				AgentID:          &agent.ID,
				TaskID:           taskIDPtr,
				RequestID:        requestID,
				Timestamp:        entry.Timestamp,
				Provider:         provider,
				Model:            normModel,
				InputTokens:      extras.Usage.Usage.InputTokens,
				OutputTokens:     extras.Usage.Usage.OutputTokens,
				CacheReadTokens:  extras.Usage.Usage.CacheReadTokens,
				CacheWriteTokens: cacheWriteTotal,
				CostMicros:       costMicros,
			}
			if err := e.Store.RecordLLMRequestCost(ctx, row); err != nil {
				// ErrConflict on the cost insert is the expected,
				// harmless path during dedup retries: the audit
				// row got resolved to the canonical id above, but
				// the cost row keyed on that same id is already
				// present from the original call. Demote to Debug
				// so routine retries don't fire alerts; surface
				// real store errors at Warn.
				if errors.Is(err, store.ErrConflict) {
					e.Logger.DebugContext(ctx, "lite-proxy: cost record deduped (canonical cost row already exists)",
						"agent_id", agent.ID, "audit_id", entry.ID)
				} else {
					e.Logger.WarnContext(ctx, "lite-proxy: cost record failed",
						"agent_id", agent.ID, "audit_id", entry.ID, "err", err.Error())
				}
			}
		}
	}
}

// WriteAuditEvent records one typed AuditEvent to the audit store.
func (e *AuditEmitter) WriteAuditEvent(ctx context.Context, agent *store.Agent, requestID string, ev conversation.AuditEvent) {
	if e == nil || e.Store == nil || agent == nil {
		return
	}
	toolInput := decodeAuditToolInput(ev.ToolUse.Input)
	params := map[string]any{
		"event":             "lite_proxy.tool_use_inspected",
		"parent_request_id": requestID,
		"tool_use_id":       ev.ToolUse.ID,
		"tool_name":         ev.ToolUse.Name,
		"tool_input":        toolInput,
		"tool_target":       toolTarget(toolInput),
		"verdict_source":    string(ev.InspectorVerdict.Source),
		"is_api_call":       ev.InspectorVerdict.IsAPICall,
		"ambiguous":         ev.InspectorVerdict.Ambiguous,
		"target_host":       ev.InspectorVerdict.Host,
		"target_method":     ev.InspectorVerdict.Method,
		"target_path":       ev.InspectorVerdict.Path,
		"placeholders":      ev.InspectorVerdict.Placeholders,
		"build_sha":         buildSHA(),
		"validator_prompt":  e.ValidatorPromptSHA,
		"parser_version":    parserVersion(),
		"clawvisor_version": version.Version,
	}
	if len(ev.InspectorVerdict.CredentialLocations) > 0 {
		creds := make([]map[string]string, 0, len(ev.InspectorVerdict.CredentialLocations))
		for _, c := range ev.InspectorVerdict.CredentialLocations {
			creds = append(creds, map[string]string{
				"kind":   c.Kind,
				"name":   c.Name,
				"scheme": c.Scheme,
			})
		}
		params["credential_locations"] = creds
	}
	for _, fact := range ev.Facts {
		if authFact, ok := fact.(conversation.AuthorizationFact); ok && authFact.Detail != "" {
			params["authorization_error"] = auditErrorDetail(authFact.Detail)
			break
		}
	}
	paramsJSON, _ := json.Marshal(params)

	service := "runtime.tool_use"
	toolUseID := ev.ToolUse.ID
	id := uuid.NewString()
	dedupKey := liteProxyEventDedupKey("tool_use_inspected", requestID, ev.ToolUse.ID)
	decision := string(ev.Decision)

	entry := &store.AuditEntry{
		ID:         id,
		UserID:     agent.UserID,
		AgentID:    &agent.ID,
		RequestID:  requestID,
		DedupKey:   &dedupKey,
		ToolUseID:  &toolUseID,
		TaskID:     nilIfEmpty(ev.TaskID),
		Timestamp:  time.Now().UTC(),
		Service:    service,
		Action:     "lite_proxy.tool_use." + decision,
		ParamsSafe: paramsJSON,
		Decision:   decision,
		Outcome:    ev.OutcomeName,
		Reason:     nilIfEmpty(ev.Reason),
	}
	if err := e.Store.LogAudit(ctx, entry); err != nil && !errors.Is(err, store.ErrConflict) {
		e.Logger.WarnContext(ctx, "lite-proxy: tool_use audit failed",
			"agent_id", agent.ID, "tool_use_id", ev.ToolUse.ID, "err", err.Error())
	}
}

// LogToolUseInspected is the legacy positional-arg API. New callers
// should use WriteAuditEvent with a typed conversation.AuditEvent.
// Kept as a thin shim for in-tree call sites + tests that haven't
// migrated yet.
func (e *AuditEmitter) LogToolUseInspected(ctx context.Context, agent *store.Agent, requestID string, tu conversation.ToolUse, verdict inspector.Verdict, decision, outcome, reason, taskID string) {
	e.WriteAuditEvent(ctx, agent, requestID, conversation.AuditEvent{
		ToolUse:          tu,
		InspectorVerdict: InspectorSnapshot(verdict),
		Decision:         conversation.DecisionKind(decision),
		OutcomeName:      outcome,
		Reason:           reason,
		TaskID:           taskID,
	})
}

func (e *AuditEmitter) LogApprovalRelease(ctx context.Context, agent *store.Agent, requestID string, pending *PendingLiteApproval, decision, outcome, reason string) {
	if e == nil || e.Store == nil || agent == nil || pending == nil {
		return
	}
	// One audit row per release event, with every held tool's
	// per-call detail under params.held_tools. The audit schema's
	// canonical dedup index is UNIQUE(user_id, request_id, COALESCE(task_id, ''))
	// which collapses N rows for the same request to one on insert;
	// emitting N rows would only land the first and silently drop
	// the rest, leaving the dashboard grouping that the comment
	// promised broken. A single row carrying the held_tools array
	// preserves the per-call detail without fighting the schema.
	holds := pending.AllHolds()
	coalesced := pending.IsCoalesced()
	heldTools := make([]map[string]any, 0, len(holds))
	for _, held := range holds {
		heldTools = append(heldTools, map[string]any{
			"tool_use_id":     held.ToolUse.ID,
			"tool_name":       held.ToolUse.Name,
			"held_kind":       string(held.Kind),
			"target_host":     held.Inspector.Host,
			"target_method":   held.Inspector.Method,
			"target_path":     held.Inspector.Path,
			"decision_source": string(held.Fingerprint.Source),
		})
	}
	// The primary held use drives the top-level target_* fields for
	// dashboards that key on (target_host, target_method, target_path)
	// without parsing held_tools. Identical to the pre-coalesce shape
	// when len(holds) == 1.
	primary := holds[0]
	if pending.IsCoalesced() && pending.PrimaryIndex < len(holds) {
		primary = holds[pending.PrimaryIndex]
	}
	params := map[string]any{
		"event":             "lite_proxy.approval_released",
		"approval_id":       pending.ID,
		"provider":          string(pending.Provider),
		"target_host":       primary.Inspector.Host,
		"target_method":     primary.Inspector.Method,
		"target_path":       primary.Inspector.Path,
		"decision_source":   string(primary.Fingerprint.Source),
		"coalesced":         coalesced,
		"hold_size":         len(holds),
		"held_tools":        heldTools,
		"build_sha":         buildSHA(),
		"validator_prompt":  e.ValidatorPromptSHA,
		"parser_version":    parserVersion(),
		"clawvisor_version": version.Version,
	}
	paramsJSON, _ := json.Marshal(params)
	tu := primary.ToolUse.ID
	id := uuid.NewString()
	dedupKey := liteProxyEventDedupKey("approval_release", requestID, pending.ID)
	entry := &store.AuditEntry{
		ID:         id,
		UserID:     agent.UserID,
		AgentID:    &agent.ID,
		RequestID:  requestID,
		DedupKey:   &dedupKey,
		ToolUseID:  &tu,
		Timestamp:  time.Now().UTC(),
		Service:    string(pending.Provider),
		Action:     "lite_proxy.approval.release",
		ParamsSafe: paramsJSON,
		Decision:   decision,
		Outcome:    outcome,
		Reason:     nilIfEmpty(reason),
	}
	if err := e.Store.LogAudit(ctx, entry); err != nil && !errors.Is(err, store.ErrConflict) {
		e.Logger.WarnContext(ctx, "lite-proxy: approval release audit failed",
			"agent_id", agent.ID, "approval_id", pending.ID, "err", err.Error())
	}
}

// LogInlineTaskApproved records the one-row audit trail for an
// inline-approved task. The row links the original tool_use that
// triggered the gesture, the created task, and the surface gesture
// (always "inline_chat") so dashboards can answer "did a human approve
// this task and how" without joining across multiple tables.
func (e *AuditEmitter) LogInlineTaskApproved(ctx context.Context, agent *store.Agent, requestID string, inner *PendingLiteApproval, task *InlineApprovedTask) {
	if e == nil || e.Store == nil || agent == nil || inner == nil || task == nil {
		return
	}
	params := map[string]any{
		"event":              "lite_proxy.task_create.inline_approved",
		"approval_id":        inner.ID,
		"awaiting_task_for":  inner.AwaitingTaskFor,
		"task_id":            task.ID,
		"approval_record_id": task.ApprovalRecordID,
		// approval_record_missing flips true when the task was created
		// but the canonical approval_records row failed to insert. The
		// task is still active — dashboards filtering by surface will
		// see it — but the audit trail is degraded. Surface explicitly
		// so monitoring can alert on the inconsistency rather than
		// guessing from an empty approval_record_id field.
		"approval_record_missing": task.ApprovalRecordID == "",
		"approval_source":         task.ApprovalSource,
		"task_status":             task.Status,
		"task_lifetime":           task.Lifetime,
		"surface":                 "inline_chat",
		"build_sha":               buildSHA(),
		"clawvisor_version":       version.Version,
	}
	paramsJSON, _ := json.Marshal(params)
	toolUseID := inner.ToolUse.ID
	id := uuid.NewString()
	dedupKey := liteProxyEventDedupKey("task_create.inline_approved", requestID, inner.ID, task.ID)
	entry := &store.AuditEntry{
		ID:         id,
		UserID:     agent.UserID,
		AgentID:    &agent.ID,
		RequestID:  requestID,
		DedupKey:   &dedupKey,
		ToolUseID:  &toolUseID,
		TaskID:     &task.ID,
		Timestamp:  time.Now().UTC(),
		Service:    "runtime.tool_use",
		Action:     "lite_proxy.task_create.inline_approved",
		ParamsSafe: paramsJSON,
		Decision:   "allow",
		Outcome:    "inline_task_approved",
	}
	if err := e.Store.LogAudit(ctx, entry); err != nil && !errors.Is(err, store.ErrConflict) {
		e.Logger.WarnContext(ctx, "lite-proxy: inline task approval audit failed",
			"agent_id", agent.ID, "approval_id", inner.ID, "task_id", task.ID, "err", err.Error())
	}
}

// LogInlineTaskAutoApproved records the audit trail for an inline task
// that bypassed the human approval prompt via the conversation-based
// auto-approval gate. Distinct from LogInlineTaskApproved because the
// trigger is fundamentally different — no human gesture was made — so
// the event name and the gate's reason are surfaced for downstream
// monitoring. Carries the same task_id / approval_record_id / tool_use
// linkage as the human-approved path so dashboards that filter by
// task_id keep working.
//
// PAIRED EMISSION: the auto-approve gate also writes a generic
// tool-use audit row (action="lite_proxy.tool_use", outcome=
// "auto_approved_from_conversation") via the rewriter's audit
// closure. This task-linked row is emitted alongside, so the audit
// trail for a single auto-approval contains TWO rows for the same
// (request_id, tool_use_id): the tool-use row records the intercept
// firing; this row records WHICH task got created. Consumers grouping
// by (request_id, tool_use_id) should expect the pair — neither row
// alone is the complete picture. Same pairing exists for the manual
// path's LogInlineTaskApproved.
func (e *AuditEmitter) LogInlineTaskAutoApproved(ctx context.Context, agent *store.Agent, requestID, toolUseID string, task *InlineApprovedTask, gateReason, riskLevel, intentMatch, threshold string) {
	if e == nil || e.Store == nil || agent == nil || task == nil {
		return
	}
	params := map[string]any{
		"event":                   "lite_proxy.task_create.inline_auto_approved",
		"task_id":                 task.ID,
		"approval_record_id":      task.ApprovalRecordID,
		"approval_record_missing": task.ApprovalRecordID == "",
		"approval_source":         task.ApprovalSource,
		"task_status":             task.Status,
		"task_lifetime":           task.Lifetime,
		"surface":                 "inline_chat_auto",
		"gate_reason":             gateReason,
		"risk_level":              riskLevel,
		"intent_match":            intentMatch,
		"threshold":               threshold,
		"build_sha":               buildSHA(),
		"clawvisor_version":       version.Version,
	}
	paramsJSON, _ := json.Marshal(params)
	id := uuid.NewString()
	dedupKey := liteProxyEventDedupKey("task_create.inline_auto_approved", requestID, toolUseID, task.ID)
	entry := &store.AuditEntry{
		ID:         id,
		UserID:     agent.UserID,
		AgentID:    &agent.ID,
		RequestID:  requestID,
		DedupKey:   &dedupKey,
		ToolUseID:  &toolUseID,
		TaskID:     &task.ID,
		Timestamp:  time.Now().UTC(),
		Service:    "runtime.tool_use",
		Action:     "lite_proxy.task_create.inline_auto_approved",
		ParamsSafe: paramsJSON,
		Decision:   "allow",
		Outcome:    "inline_task_auto_approved",
	}
	if err := e.Store.LogAudit(ctx, entry); err != nil && !errors.Is(err, store.ErrConflict) {
		e.Logger.WarnContext(ctx, "lite-proxy: inline task auto-approval audit failed",
			"agent_id", agent.ID, "task_id", task.ID, "err", err.Error())
	}
}

// LogContinuationSkippedSiblingTools records the audit trail for the
// case where the conversation auto-approval gate fired AND created a
// task, but the model's assistant turn carried sibling tool_uses that
// were NOT auto-approved (count mismatch between tool_uses and
// continuation tool_results). The continuation was skipped to avoid
// double-execution of the sibling tool_uses; the substitute fallback
// rendered as a terminal assistant turn instead. The sibling
// tool_uses were dropped from that rendered turn — the model never
// receives a tool_result for them and the harness never executes
// them. This is the audit row an operator chasing "I approved the
// task but Bash never ran" can grep for.
//
// droppedToolNames is the list of sibling tool names the model
// emitted alongside the auto-approved POST /api/control/tasks; the
// AUDIT row records them by name (not arguments — those can be large
// and may carry sensitive payloads).
func (e *AuditEmitter) LogContinuationSkippedSiblingTools(ctx context.Context, agent *store.Agent, requestID string, taskID, autoApprovedToolUseID string, droppedToolNames []string) {
	if e == nil || e.Store == nil || agent == nil {
		return
	}
	// Normalize a nil dropped-names slice to []string{} so the JSON
	// renders as `[]` not `null`. Keeps the row's shape consistent
	// with the documented dropped_count=0 companion field and saves
	// downstream consumers a null-vs-empty branch.
	if droppedToolNames == nil {
		droppedToolNames = []string{}
	}
	params := map[string]any{
		"event":                  "lite_proxy.continuation.skipped_sibling_tools",
		"task_id":                taskID,
		"auto_approved_tool_use": autoApprovedToolUseID,
		"dropped_tool_names":     droppedToolNames,
		"dropped_count":          len(droppedToolNames),
		"reason":                 "would unbalance tool_use/tool_result count; substitute fallback rendered instead and sibling tool_uses were not executed",
		"build_sha":              buildSHA(),
		"clawvisor_version":      version.Version,
	}
	paramsJSON, _ := json.Marshal(params)
	var taskIDPtr *string
	if taskID != "" {
		t := taskID
		taskIDPtr = &t
	}
	var toolUseIDPtr *string
	if autoApprovedToolUseID != "" {
		id := autoApprovedToolUseID
		toolUseIDPtr = &id
	}
	id := uuid.NewString()
	dedupKey := liteProxyEventDedupKey("continuation.skipped_sibling_tools", requestID, taskID, autoApprovedToolUseID)
	entry := &store.AuditEntry{
		ID:         id,
		UserID:     agent.UserID,
		AgentID:    &agent.ID,
		RequestID:  requestID,
		DedupKey:   &dedupKey,
		ToolUseID:  toolUseIDPtr,
		TaskID:     taskIDPtr,
		Timestamp:  time.Now().UTC(),
		Service:    "runtime.tool_use",
		Action:     "lite_proxy.continuation.skipped_sibling_tools",
		ParamsSafe: paramsJSON,
		Decision:   "allow",
		Outcome:    "continuation_skipped_sibling_tools",
	}
	if err := e.Store.LogAudit(ctx, entry); err != nil && !errors.Is(err, store.ErrConflict) {
		e.Logger.WarnContext(ctx, "lite-proxy: continuation-skipped audit failed",
			"agent_id", agent.ID, "request_id", requestID, "err", err.Error())
	}
}

// LogResolverSwap records one credential swap at the resolver. Each row
// links to the placeholder, target host, and upstream status.
func (e *AuditEmitter) LogResolverSwap(ctx context.Context, agent *store.Agent, requestID, placeholder, boundService, targetHost, targetPath, method string, statusCode int, decision, outcome, reason string, duration time.Duration) {
	if e == nil || e.Store == nil || agent == nil {
		return
	}
	params := map[string]any{
		"event":             "lite_proxy.resolver_swap",
		"placeholder":       placeholder,
		"bound_service":     boundService,
		"target_host":       targetHost,
		"target_path":       targetPath,
		"method":            method,
		"http_status":       statusCode,
		"build_sha":         buildSHA(),
		"clawvisor_version": version.Version,
	}
	paramsJSON, _ := json.Marshal(params)
	id := uuid.NewString()
	dedupKey := liteProxyEventDedupKey("resolver_swap", requestID, placeholder, boundService, targetHost, targetPath, method)
	entry := &store.AuditEntry{
		ID:         id,
		UserID:     agent.UserID,
		AgentID:    &agent.ID,
		RequestID:  requestID,
		DedupKey:   &dedupKey,
		Timestamp:  time.Now().UTC(),
		Service:    boundService,
		Action:     "lite_proxy.resolver." + method,
		ParamsSafe: paramsJSON,
		Decision:   decision,
		Outcome:    outcome,
		Reason:     nilIfEmpty(reason),
		DurationMS: int(duration.Milliseconds()),
	}
	if err := e.Store.LogAudit(ctx, entry); err != nil && !errors.Is(err, store.ErrConflict) {
		e.Logger.WarnContext(ctx, "lite-proxy: resolver swap audit failed",
			"agent_id", agent.ID, "target_host", targetHost, "err", err.Error())
	}
}

// LogScriptSessionMint records one autovault script-session mint. The
// row captures the structured capability the verifier evaluated against
// (placeholder, host, methods, prefixes, caps) so a future incident
// reconstruction can see exactly what the agent was granted.
func (e *AuditEmitter) LogScriptSessionMint(ctx context.Context, agent *store.Agent, sess ScriptSession, statusCode int, decision, outcome, reason string) {
	if e == nil || e.Store == nil || agent == nil {
		return
	}
	params := map[string]any{
		"event":             "lite_proxy.script_session.mint",
		"script_session_id": sess.ID,
		"task_id":           sess.TaskID,
		"placeholder":       sess.Placeholder,
		"bound_service":     sess.ServiceID,
		"target_host":       sess.TargetHost,
		"methods":           sess.Methods,
		"path_prefixes":     sess.PathPrefixes,
		"max_uses":          sess.MaxUses,
		"max_request_bytes": sess.MaxRequestBytes,
		"max_total_bytes":   sess.MaxTotalBytes,
		"expires_at":        sess.ExpiresAt.UTC().Format(time.RFC3339),
		"why":               sess.Why,
		"http_status":       statusCode,
		"build_sha":         buildSHA(),
		"clawvisor_version": version.Version,
	}
	paramsJSON, _ := json.Marshal(params)
	taskIDPtr := nilIfEmpty(sess.TaskID)
	// DedupKey: keyed on (session_id, outcome) for successful mints
	// (sess.ID is the unique session UUID). For deny-path rows
	// where sess.ID is empty (mint failed before session creation —
	// e.g. invalid_json, placeholder_required), the (sess.ID,
	// outcome) tuple collapses to ("", outcome) and ALL distinct
	// denials of the same kind by the same user would dedupe to a
	// single row. Tiebreak with a fresh UUID in that case — the
	// row is effectively un-deduped, which is the right outcome
	// for distinct denial attempts.
	dedupSessKey := sess.ID
	if dedupSessKey == "" {
		dedupSessKey = uuid.NewString()
	}
	dedupKey := liteProxyEventDedupKey("script_session_mint", dedupSessKey, outcome)
	entry := &store.AuditEntry{
		ID:         uuid.NewString(),
		UserID:     agent.UserID,
		AgentID:    &agent.ID,
		TaskID:     taskIDPtr,
		DedupKey:   &dedupKey,
		Timestamp:  time.Now().UTC(),
		Service:    sess.ServiceID,
		Action:     "autovault.script_session.mint",
		ParamsSafe: paramsJSON,
		Decision:   decision,
		Outcome:    outcome,
		Reason:     nilIfEmpty(reason),
	}
	if err := e.Store.LogAudit(ctx, entry); err != nil && !errors.Is(err, store.ErrConflict) {
		e.Logger.WarnContext(ctx, "lite-proxy: script-session mint audit failed",
			"agent_id", agent.ID, "session_id", sess.ID, "err", err.Error())
	}
}

// LogScriptSessionUse records one resolver request authorized by a
// script-session token. Each row links to the parent session id so the
// dashboard can roll up uses + bytes per session. resp_bytes is the
// upstream response size in bytes; total_bytes is the post-update
// aggregate as returned by ScriptSessionCache.RecordBytes.
func (e *AuditEmitter) LogScriptSessionUse(ctx context.Context, agent *store.Agent, requestID string, sess ScriptSession, targetPath, method string, statusCode int, decision, outcome, reason string, respBytes, totalBytes int64, useCount int, duration time.Duration) {
	if e == nil || e.Store == nil || agent == nil {
		return
	}
	params := map[string]any{
		"event":             "lite_proxy.script_session.use",
		"script_session_id": sess.ID,
		"task_id":           sess.TaskID,
		"placeholder":       sess.Placeholder,
		"bound_service":     sess.ServiceID,
		"target_host":       sess.TargetHost,
		"target_path":       targetPath,
		"method":            method,
		"http_status":       statusCode,
		"use_count":         useCount,
		"max_uses":          sess.MaxUses,
		"resp_bytes":        respBytes,
		"total_bytes":       totalBytes,
		"max_total_bytes":   sess.MaxTotalBytes,
		"build_sha":         buildSHA(),
		"clawvisor_version": version.Version,
	}
	paramsJSON, _ := json.Marshal(params)
	// DedupKey: keyed on (request_id, session_id, path, method) to
	// dedupe retries of the SAME logical request. When requestID is
	// empty (inbound request had no X-Request-Id header), all
	// concurrent uses of the same session against the same path
	// would otherwise collide on one dedup bucket and the store
	// would drop legitimate distinct rows. Use the audit row's own
	// UUID as a tiebreaker in that case — it disables dedup but
	// preserves correctness.
	dedupReqKey := requestID
	if dedupReqKey == "" {
		dedupReqKey = uuid.NewString()
	}
	dedupKey := liteProxyEventDedupKey("script_session_use", dedupReqKey, sess.ID, targetPath, method)
	// TaskID on the column (not just ParamsSafe JSON) so a "show all
	// activity for task X" query joins these rows alongside the mint
	// row. Mirrors LogScriptSessionMint's column population.
	entry := &store.AuditEntry{
		ID:         uuid.NewString(),
		UserID:     agent.UserID,
		AgentID:    &agent.ID,
		TaskID:     nilIfEmpty(sess.TaskID),
		RequestID:  requestID,
		DedupKey:   &dedupKey,
		Timestamp:  time.Now().UTC(),
		Service:    sess.ServiceID,
		Action:     "autovault.script_session.use." + method,
		ParamsSafe: paramsJSON,
		Decision:   decision,
		Outcome:    outcome,
		Reason:     nilIfEmpty(reason),
		DurationMS: int(duration.Milliseconds()),
	}
	if err := e.Store.LogAudit(ctx, entry); err != nil && !errors.Is(err, store.ErrConflict) {
		e.Logger.WarnContext(ctx, "lite-proxy: script-session use audit failed",
			"agent_id", agent.ID, "session_id", sess.ID, "err", err.Error())
	}
}

func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// errString safely renders an error for slog without panicking on nil.
// Used on defensive branches where the error may or may not be set
// (e.g. a nil-return-with-nil-error contract from a store lookup).
func errString(err error) string {
	if err == nil {
		return "<nil>"
	}
	return err.Error()
}

func liteProxyEventDedupKey(kind string, parts ...string) string {
	h := sha256.New()
	_, _ = h.Write([]byte(strings.TrimSpace(kind)))
	for _, part := range parts {
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(strings.TrimSpace(part)))
	}
	return "lite_proxy_event:" + strings.TrimSpace(kind) + ":" + hex.EncodeToString(h.Sum(nil))
}

// buildSHA returns the clawvisor build identifier. Stamped at link time
// via -ldflags; falls back to "unknown".
func buildSHA() string {
	return version.Version
}

// parserVersion returns a stable identifier for the deterministic
// parser implementation in this build. Bump when parsing semantics
// change; recorded in audit rows so verdict differences across builds
// are forensically visible.
const parserVersionStr = "lite-proxy-parser/v1"

func parserVersion() string { return parserVersionStr }

func decodeAuditToolInput(raw json.RawMessage) map[string]any {
	if len(raw) == 0 {
		return nil
	}
	var input map[string]any
	if err := json.Unmarshal(raw, &input); err != nil {
		return map[string]any{"raw": "<unparseable>"}
	}
	return truncateAuditMap(input, 512)
}

func truncateAuditMap(input map[string]any, maxString int) map[string]any {
	if len(input) == 0 {
		return nil
	}
	out := make(map[string]any, len(input))
	for k, v := range input {
		if isAuditSecretKey(k) {
			continue
		}
		out[k] = truncateAuditValue(v, maxString)
	}
	return out
}

func truncateAuditValue(v any, maxString int) any {
	switch t := v.(type) {
	case string:
		return truncateAuditString(t, maxString)
	case map[string]any:
		return truncateAuditMap(t, maxString)
	case []any:
		out := make([]any, 0, len(t))
		for i, item := range t {
			if i >= 20 {
				out = append(out, "...<truncated>")
				break
			}
			out = append(out, truncateAuditValue(item, maxString))
		}
		return out
	default:
		return v
	}
}

func truncateAuditString(s string, max int) string {
	s = redactSecretsInString(s)
	if max <= 0 || len(s) <= max {
		return s
	}
	return s[:max] + "...<truncated>"
}

func auditErrorDetail(s string) string {
	return SafeAuditErrorDetail(s)
}

// SafeAuditErrorDetail returns a bounded, credential-redacted error
// string suitable for audit params.
func SafeAuditErrorDetail(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		if r < 0x20 {
			return -1
		}
		return r
	}, redactSecretsInString(s))
	return truncateAuditString(s, 512)
}

// auditSecretValueRE matches the credential patterns we don't want
// landing in audit rows, even when they appear embedded in larger
// strings such as a `command` field's shell-command-line value or a
// `url` value with basic-auth credentials.
//
// Patterns covered:
//   - Bearer tokens: `Bearer <token>` (any non-whitespace token).
//   - Known prefixed credentials: `sk-ant-...`, `sk-...`, `ghp_...`,
//     `gho_...`, `ghu_...`, `ghs_...`, `xoxb-...`, `xoxa-...`, `xoxp-...`,
//     `cvis_...`.
//   - URL embedded basic auth: `https://user:secret@host/...`.
//   - Autovault placeholders are NOT redacted — they're tokens by
//     reference, not by value, and operators rely on seeing them.
var auditSecretValueRE = regexp.MustCompile(
	`(?i)` +
		// Bearer token: terminate match at whitespace OR quote char so we
		// don't swallow the closing ' or " when the token sat inside a
		// quoted shell argument.
		`(?:bearer\s+[^\s'"]+|` +
		`sk-ant-[A-Za-z0-9_-]+|` +
		`sk-(?:proj-)?[A-Za-z0-9_-]{8,}|` +
		`github_pat_[A-Za-z0-9_]+|` +
		`ghp_[A-Za-z0-9]+|gho_[A-Za-z0-9]+|ghu_[A-Za-z0-9]+|ghs_[A-Za-z0-9]+|ghr_[A-Za-z0-9]+|` +
		`xox[abp]-[A-Za-z0-9-]+|` +
		`cvis_[A-Za-z0-9]+` +
		`)` +
		`|(https?://)[^/:@\s]+:[^@\s]+@`)

// redactSecretsInString replaces well-known credential patterns with
// `<REDACTED:auth>`. Applied to every string value flowing into the
// audit row so credentials embedded in command-line or URL strings
// don't survive into the audit log. Key-based filtering at the
// caller is necessary but not sufficient — values can carry secrets
// in fields not named like secrets (e.g. `command`).
func redactSecretsInString(s string) string {
	if s == "" {
		return s
	}
	return auditSecretValueRE.ReplaceAllStringFunc(s, func(match string) string {
		// URL basic-auth case: preserve scheme prefix.
		if strings.HasPrefix(match, "http://") || strings.HasPrefix(match, "https://") {
			scheme := "https://"
			if strings.HasPrefix(match, "http://") {
				scheme = "http://"
			}
			return scheme + "<REDACTED:auth>@"
		}
		return "<REDACTED:auth>"
	})
}

func toolTarget(input map[string]any) string {
	if len(input) == 0 {
		return ""
	}
	for _, key := range []string{"url", "file_path", "path", "directory", "pattern", "command"} {
		if v, ok := input[key].(string); ok && strings.TrimSpace(v) != "" {
			return truncateAuditString(strings.TrimSpace(v), 512)
		}
	}
	return ""
}

func isAuditSecretKey(key string) bool {
	key = strings.ToLower(strings.TrimSpace(key))
	if key == "" {
		return false
	}
	for _, marker := range []string{"authorization", "api_key", "apikey", "access_key", "private_key", "token", "secret", "password", "bearer"} {
		if strings.Contains(key, marker) {
			return true
		}
	}
	return false
}
