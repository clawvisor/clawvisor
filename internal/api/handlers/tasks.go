package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/clawvisor/clawvisor/internal/adapters/google/credential"
	"github.com/clawvisor/clawvisor/internal/api/middleware"
	"github.com/clawvisor/clawvisor/internal/callback"
	"github.com/clawvisor/clawvisor/internal/events"
	"github.com/clawvisor/clawvisor/internal/groupchat"
	"github.com/clawvisor/clawvisor/internal/intent"
	"github.com/clawvisor/clawvisor/internal/llm"
	runtimeautovault "github.com/clawvisor/clawvisor/internal/runtime/autovault"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy"
	runtimepolicy "github.com/clawvisor/clawvisor/internal/runtime/policy"
	runtimetasks "github.com/clawvisor/clawvisor/internal/runtime/tasks"
	"github.com/clawvisor/clawvisor/internal/taskrisk"
	"github.com/clawvisor/clawvisor/pkg/adapters"
	"github.com/clawvisor/clawvisor/pkg/config"
	"github.com/clawvisor/clawvisor/pkg/notify"
	"github.com/clawvisor/clawvisor/pkg/store"
	"github.com/clawvisor/clawvisor/pkg/vault"
)

// hardcodedApprovalActions lists service:action pairs that always require
// per-request human approval, regardless of policy or task scope.
var hardcodedApprovalActions = map[string]bool{
	"apple.imessage:send_message": true,
}

// RequiresHardcodedApproval returns true if the given service+action always
// requires individual human approval.
func RequiresHardcodedApproval(service, action string) bool {
	return hardcodedApprovalActions[service+":"+action]
}

// TasksHandler manages task-scoped authorization.
type TasksHandler struct {
	st               store.Store
	vault            vault.Vault
	adapterReg       *adapters.Registry
	notifier         notify.Notifier
	cfg              config.Config
	logger           *slog.Logger
	baseURL          string
	eventHub         events.EventHub
	assessor         taskrisk.Assessor
	contentDedup     DedupCache
	msgBuffer        groupchat.Buffer        // may be nil; set via SetGroupApproval
	llmHealth        *llm.Health             // may be nil; needed for approval check LLM calls
	agentPairer      notify.AgentGroupPairer // may be nil; set via SetGroupApproval
	localSvcProvider LocalServiceProvider    // may be nil; set via SetLocalServiceProvider
	cbDispatch       *CallbackDispatcher     // bounded callback delivery; may be nil
}

// SetCallbackDispatcher wires a bounded callback delivery pool. When unset,
// callback delivery falls back to a safeGo-wrapped inline goroutine.
func (h *TasksHandler) SetCallbackDispatcher(d *CallbackDispatcher) {
	h.cbDispatch = d
}

// dispatchCallback enqueues a payload for delivery via the bounded
// dispatcher when available, or spawns a panic-safe goroutine otherwise.
func (h *TasksHandler) dispatchCallback(url string, payload *callback.Payload, signingKey string) {
	if url == "" || payload == nil {
		return
	}
	if h.cbDispatch != nil {
		h.cbDispatch.Submit(url, payload, signingKey)
		return
	}
	safeGo(h.logger, "callback delivery (inline)", func() {
		_ = callback.DeliverResult(context.Background(), url, payload, signingKey)
	})
}

// SetLocalServiceProvider configures the local daemon service provider for
// validating local service names during task creation and expansion.
func (h *TasksHandler) SetLocalServiceProvider(p LocalServiceProvider) {
	h.localSvcProvider = p
}

func NewTasksHandler(
	st store.Store,
	v vault.Vault,
	adapterReg *adapters.Registry,
	notifier notify.Notifier,
	cfg config.Config,
	logger *slog.Logger,
	baseURL string,
	eventHub events.EventHub,
	assessor taskrisk.Assessor,
) *TasksHandler {
	dedupTTL := time.Duration(cfg.Gateway.ContentDedupTTLSeconds) * time.Second
	if dedupTTL <= 0 {
		dedupTTL = 5 * time.Second
	}
	return &TasksHandler{
		st: st, vault: v, adapterReg: adapterReg, notifier: notifier, cfg: cfg, logger: logger, baseURL: baseURL,
		eventHub: eventHub, assessor: assessor,
		contentDedup: newDedupCache(dedupTTL),
	}
}

// SetGroupApproval configures the message buffer, LLM health, and agent-group
// pairer used for on-demand group chat approval checks during task creation.
// SetDedupCache overrides the default in-memory content dedup cache.
func (h *TasksHandler) SetDedupCache(dc DedupCache) {
	h.contentDedup = dc
}

func (h *TasksHandler) SetGroupApproval(buf groupchat.Buffer, health *llm.Health, pairer notify.AgentGroupPairer) {
	h.msgBuffer = buf
	h.llmHealth = health
	h.agentPairer = pairer
}

// ── Create ────────────────────────────────────────────────────────────────────

type createTaskRequest struct {
	Purpose                string                            `json:"purpose"`
	AuthorizedActions      []store.TaskAction                `json:"authorized_actions"`
	PlannedCalls           []store.PlannedCall               `json:"planned_calls,omitempty"`
	ExpectedTools          []runtimetasks.ExpectedTool       `json:"expected_tools,omitempty"`
	ExpectedEgress         []runtimetasks.ExpectedEgress     `json:"expected_egress,omitempty"`
	RequiredCredentials    []runtimetasks.RequiredCredential `json:"required_credentials,omitempty"`
	IntentVerificationMode string                            `json:"intent_verification_mode,omitempty"`
	ChainExtractionMode    string                            `json:"chain_extraction_mode,omitempty"` // "" | "full" | "builtins_only"
	ExpectedUse            string                            `json:"expected_use,omitempty"`
	SchemaVersion          int                               `json:"schema_version,omitempty"`
	ExpiresInSeconds       int                               `json:"expires_in_seconds"`
	CallbackURL            string                            `json:"callback_url"`
	Lifetime               string                            `json:"lifetime"` // "session" (default), "sliding", or "standing"
}

// Create declares a new task scope.
//
// POST /api/tasks
// Auth: agent bearer token
func (h *TasksHandler) Create(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	agent := middleware.AgentFromContext(ctx)
	if agent == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}

	var req createTaskRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	// Collect all missing top-level required fields at once so the caller
	// can fix everything in a single round-trip.
	hasRuntimeEnvelope := len(req.ExpectedTools) > 0 || len(req.ExpectedEgress) > 0
	hasCredentialRequests := len(req.RequiredCredentials) > 0
	hasV2Fields := hasRuntimeEnvelope || hasCredentialRequests
	var missingFields []string
	if req.Purpose == "" {
		missingFields = append(missingFields, "purpose")
	}
	if len(req.AuthorizedActions) == 0 && !hasRuntimeEnvelope {
		missingFields = append(missingFields, "authorized_actions", "expected_tools", "expected_egress")
	}
	if len(missingFields) > 0 {
		writeDetailedError(w, http.StatusBadRequest, apiErrorDetail{
			Error:         "missing required fields: " + strings.Join(missingFields, ", "),
			Code:          "INVALID_REQUEST",
			Hint:          "A task requires a purpose describing what the agent will do and at least one scope representation: authorized_actions, expected_tools, or expected_egress.",
			MissingFields: missingFields,
			Example: map[string]any{
				"purpose": "Read and summarize recent emails",
				"authorized_actions": []map[string]any{
					{"service": "google.gmail", "action": "list_messages", "auto_execute": true},
				},
			},
		})
		return
	}
	if len(req.PlannedCalls) > 0 && len(req.AuthorizedActions) == 0 {
		// Pre-merge the gateway tolerated planned_calls without authorized_actions
		// (the planned calls were stored as risk-assessment metadata and the
		// scope was inferred elsewhere). To avoid breaking existing clients,
		// auto-derive a scope from the planned call (service, action) pairs and
		// log a deprecation. Callers should send authorized_actions explicitly —
		// this fallback may be removed in a future release.
		seen := make(map[string]struct{}, len(req.PlannedCalls))
		for _, pc := range req.PlannedCalls {
			service := strings.TrimSpace(pc.Service)
			action := strings.TrimSpace(pc.Action)
			if service == "" || action == "" {
				continue
			}
			key := service + ":" + action
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			req.AuthorizedActions = append(req.AuthorizedActions, store.TaskAction{
				Service:     service,
				Action:      action,
				AutoExecute: false,
			})
		}
		if len(req.AuthorizedActions) == 0 {
			writeDetailedError(w, http.StatusBadRequest, apiErrorDetail{
				Error: "planned_calls requires authorized_actions",
				Code:  "INVALID_REQUEST",
				Hint:  "Each planned_call must have non-empty service and action, or set authorized_actions explicitly.",
			})
			return
		}
		h.logger.WarnContext(ctx, "deprecated: task created with planned_calls but no authorized_actions; deriving scope from planned_calls",
			"agent_id", agent.ID,
			"derived_scope_count", len(req.AuthorizedActions),
		)
	}

	schemaVersion := req.SchemaVersion
	if schemaVersion == 0 {
		if hasV2Fields || req.IntentVerificationMode != "" || req.ExpectedUse != "" {
			schemaVersion = 2
		} else {
			schemaVersion = 1
		}
	}
	if schemaVersion != 1 && schemaVersion != 2 {
		writeDetailedError(w, http.StatusBadRequest, apiErrorDetail{
			Error:     fmt.Sprintf("invalid schema_version %d", req.SchemaVersion),
			Code:      "INVALID_REQUEST",
			Hint:      "schema_version must be 1 or 2.",
			Available: []string{"1", "2"},
		})
		return
	}
	if schemaVersion == 1 && (hasV2Fields || req.IntentVerificationMode != "" || req.ExpectedUse != "") {
		writeDetailedError(w, http.StatusBadRequest, apiErrorDetail{
			Error: "schema_version=1 cannot be used with v2 task envelope fields",
			Code:  "INVALID_REQUEST",
			Hint:  "Use schema_version 2 when sending expected_tools, expected_egress, required_credentials, intent_verification_mode, or expected_use.",
		})
		return
	}

	env := runtimetasks.Envelope{
		ExpectedTools:          req.ExpectedTools,
		ExpectedEgress:         req.ExpectedEgress,
		RequiredCredentials:    req.RequiredCredentials,
		IntentVerificationMode: req.IntentVerificationMode,
		ExpectedUse:            req.ExpectedUse,
		SchemaVersion:          schemaVersion,
	}
	if hasV2Fields && env.IntentVerificationMode == "" {
		env.IntentVerificationMode = "strict"
	}
	if hasRuntimeEnvelope {
		if issues := runtimepolicy.ValidateTaskEnvelope(env); len(issues) > 0 {
			var messages []string
			var fields []string
			for _, issue := range issues {
				messages = append(messages, issue.Field+": "+issue.Message)
				fields = append(fields, issue.Field)
			}
			writeDetailedError(w, http.StatusBadRequest, apiErrorDetail{
				Error:         strings.Join(messages, "; "),
				Code:          "INVALID_REQUEST",
				MissingFields: fields,
				Hint:          "Task envelope v2 items must declare specific tools or egress targets with valid shapes and human-readable why fields.",
			})
			return
		}
	}
	if hasCredentialRequests && !hasRuntimeEnvelope {
		if issues := runtimepolicy.ValidateRequiredCredentials(req.RequiredCredentials); len(issues) > 0 {
			var messages []string
			var fields []string
			for _, issue := range issues {
				messages = append(messages, issue.Field+": "+issue.Message)
				fields = append(fields, issue.Field)
			}
			writeDetailedError(w, http.StatusBadRequest, apiErrorDetail{
				Error:         strings.Join(messages, "; "),
				Code:          "INVALID_REQUEST",
				MissingFields: fields,
				Hint:          "Credential requests must name a concrete vault item and explain why the task needs it.",
			})
			return
		}
	}

	// Fail fast on credential handles that aren't in the vault. Without
	// this, the task lands in pending_approval and the user is asked to
	// approve something that can't possibly authorize — and the
	// validation error never reaches the agent (it fires at release
	// time, after the agent's POST already got back "pending_approval").
	// validateTaskRequiredCredentials only reads UserID + AgentID off
	// the task, so a stub is sufficient pre-persistence. Matches the
	// proxy-intercept submission path in CreatePendingInlineTask, which
	// has done this check since inline tasks shipped.
	if hasCredentialRequests {
		stubTask := &store.Task{UserID: agent.UserID, AgentID: agent.ID}
		if err := h.validateTaskRequiredCredentials(ctx, stubTask, req.RequiredCredentials); err != nil {
			writeError(w, http.StatusBadRequest, "INVALID_CREDENTIAL_REQUEST", err.Error())
			return
		}
	}

	// Validate each authorized action.
	for i, a := range req.AuthorizedActions {
		// Validate that each action entry has the required service and action fields.
		if a.Service == "" || a.Action == "" {
			var actionMissing []string
			if a.Service == "" {
				actionMissing = append(actionMissing, "service")
			}
			if a.Action == "" {
				actionMissing = append(actionMissing, "action")
			}
			writeDetailedError(w, http.StatusBadRequest, apiErrorDetail{
				Error:         fmt.Sprintf("authorized_actions[%d] is missing required fields: %s", i, strings.Join(actionMissing, ", ")),
				Code:          "INVALID_REQUEST",
				MissingFields: actionMissing,
				Hint:          "Each authorized action must specify a service ID and an action name (or \"*\" for all actions).",
				Example: map[string]any{
					"service": "google.gmail", "action": "list_messages", "auto_execute": true,
				},
			})
			return
		}

		serviceType, serviceAlias := parseServiceAlias(a.Service)

		// Validate local daemon services against the active service list.
		if isLocalService(serviceType) {
			if err := h.validateLocalService(ctx, agent.UserID, serviceType, a.Action); err != nil {
				writeError(w, http.StatusBadRequest, "INVALID_REQUEST", err.Error())
				return
			}
		}

		// Guard virtual services and local daemon services skip adapter/activation
		// validation — they don't have adapters in the registry.
		if !isGuardVirtualService(serviceType) && !isLocalService(serviceType) {
			adapter, ok := h.adapterReg.GetForUser(ctx, serviceType, agent.UserID)
			if !ok {
				available := h.adapterReg.SupportedServices()
				var ids []string
				for _, s := range available {
					ids = append(ids, s.ID)
				}
				writeDetailedError(w, http.StatusBadRequest, apiErrorDetail{
					Error:     fmt.Sprintf("unknown service %q in authorized_actions[%d]", a.Service, i),
					Code:      "INVALID_REQUEST",
					Hint:      "Use the service ID from the catalog (GET /api/skill/catalog). Service IDs look like \"google.gmail\", not display names like \"Gmail\".",
					Available: ids,
				})
				return
			}
			if a.Action != "*" && !adapterSupportsAction(adapter, a.Action) {
				writeDetailedError(w, http.StatusBadRequest, apiErrorDetail{
					Error:     fmt.Sprintf("service %q does not support action %q", serviceType, a.Action),
					Code:      "INVALID_REQUEST",
					Hint:      fmt.Sprintf("Use \"*\" to authorize all actions, or pick from the supported actions for this service."),
					Available: adapter.SupportedActions(),
				})
				return
			}
			if !h.serviceActivated(ctx, agent.UserID, serviceType, serviceAlias, adapter) {
				code, userErr, _ := serviceNotActivatedResponse(ctx, h.vault, h.st, h.adapterReg, agent.UserID, serviceType, serviceAlias, a.Service, adapter)
				writeError(w, http.StatusBadRequest, code, userErr)
				return
			}
			if missing := h.missingCredentialScopes(ctx, agent.UserID, serviceType, serviceAlias, a.Action, adapter); len(missing) > 0 {
				writeError(w, http.StatusBadRequest, "MISSING_SCOPES",
					fmt.Sprintf("service %q is connected but missing required OAuth scopes: %s — "+
						"the user needs to reconnect the service to grant these permissions",
						a.Service, strings.Join(missing, ", ")))
				return
			}
		}
		if a.AutoExecute && RequiresHardcodedApproval(a.Service, a.Action) {
			writeDetailedError(w, http.StatusBadRequest, apiErrorDetail{
				Error: fmt.Sprintf("action %s:%s requires per-request human approval — auto_execute must be false", a.Service, a.Action),
				Code:  "INVALID_REQUEST",
				Hint:  "Some actions (like sending iMessages) always require individual approval for safety. Set auto_execute to false for this action.",
				Example: map[string]any{
					"service": a.Service, "action": a.Action, "auto_execute": false,
				},
			})
			return
		}
		if a.Verification != "" && a.Verification != "strict" && a.Verification != "lenient" && a.Verification != "off" {
			writeDetailedError(w, http.StatusBadRequest, apiErrorDetail{
				Error: fmt.Sprintf("authorized_actions[%d].verification %q is invalid", i, a.Verification),
				Code:  "INVALID_VERIFICATION_MODE",
				Hint:  "verification must be one of: strict, lenient, off (or omitted, which defaults to strict).",
				Example: map[string]any{
					"service": a.Service, "action": a.Action, "verification": "strict",
				},
			})
			return
		}
	}

	// Validate planned calls: each must reference a service:action covered by authorized_actions.
	for i, pc := range req.PlannedCalls {
		var pcMissing []string
		if pc.Service == "" {
			pcMissing = append(pcMissing, "service")
		}
		if pc.Action == "" {
			pcMissing = append(pcMissing, "action")
		}
		if pc.Reason == "" {
			pcMissing = append(pcMissing, "reason")
		}
		if len(pcMissing) > 0 {
			writeDetailedError(w, http.StatusBadRequest, apiErrorDetail{
				Error:         fmt.Sprintf("planned_calls[%d] is missing required fields: %s", i, strings.Join(pcMissing, ", ")),
				Code:          "INVALID_REQUEST",
				MissingFields: pcMissing,
				Hint:          "Each planned call must specify the service, action, and a reason explaining why this call will be made.",
				Example: map[string]any{
					"service": "google.gmail", "action": "send_message", "reason": "Send the daily summary email to the user",
				},
			})
			return
		}
		covered := false
		pcServiceType, _ := parseServiceAlias(pc.Service)
		for _, a := range req.AuthorizedActions {
			aServiceType, _ := parseServiceAlias(a.Service)
			if aServiceType == pcServiceType && (a.Action == pc.Action || a.Action == "*") {
				covered = true
				break
			}
		}
		if !covered {
			var authorizedStrs []string
			for _, a := range req.AuthorizedActions {
				authorizedStrs = append(authorizedStrs, a.Service+":"+a.Action)
			}
			writeDetailedError(w, http.StatusBadRequest, apiErrorDetail{
				Error:     fmt.Sprintf("planned_calls[%d] (%s:%s) is not covered by authorized_actions", i, pc.Service, pc.Action),
				Code:      "INVALID_REQUEST",
				Hint:      "Every planned call must match a service:action pair (or wildcard) in authorized_actions. Add the missing action or use \"*\" as the action.",
				Available: authorizedStrs,
			})
			return
		}
	}

	lifetime := req.Lifetime
	if lifetime == "" {
		lifetime = "session"
	}
	if lifetime != "session" && lifetime != "standing" && lifetime != "sliding" {
		writeDetailedError(w, http.StatusBadRequest, apiErrorDetail{
			Error:     fmt.Sprintf("invalid lifetime %q", req.Lifetime),
			Code:      "INVALID_REQUEST",
			Hint:      "Session tasks expire after a fixed timeout. Sliding tasks expire after a timeout that auto-extends on each authorized tool_use. Standing tasks persist until revoked.",
			Available: []string{"session", "sliding", "standing"},
		})
		return
	}
	if lifetime == "standing" && req.ExpiresInSeconds > 0 {
		writeDetailedError(w, http.StatusBadRequest, apiErrorDetail{
			Error: "expires_in_seconds cannot be set on a standing task",
			Code:  "INVALID_REQUEST",
			Hint:  "Standing tasks have no expiry — they remain active until explicitly revoked via POST /api/tasks/{id}/revoke. Remove expires_in_seconds or change lifetime to \"session\".",
		})
		return
	}

	// chain_extraction_mode is a small enum; reject unknown values before the
	// dedup lookup so an invalid value can't surface a cached 201 response,
	// and so the mode participates correctly in the dedup key below.
	switch req.ChainExtractionMode {
	case "", "full", "builtins_only":
	default:
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST",
			`chain_extraction_mode must be "", "full", or "builtins_only"`)
		return
	}

	// Content-based dedup: if an identical task creation request was recently made
	// by the same agent, return the existing task instead of creating a duplicate.
	//
	// v2 envelope fields (planned_calls, expected_tools, expected_egress,
	// intent_verification_mode, expected_use, schema_version) only participate in
	// the hash when any are non-empty. v1-only requests therefore produce the
	// same fingerprint they did pre-#310, preserving the existing dedup window.
	// chain_extraction_mode is treated the same way: only enters the key when
	// set, so default tasks keep their existing fingerprint.
	dedupParts := []any{"task", agent.ID, req.Purpose, req.AuthorizedActions}
	if len(req.PlannedCalls) > 0 ||
		len(req.ExpectedTools) > 0 ||
		len(req.ExpectedEgress) > 0 ||
		len(req.RequiredCredentials) > 0 ||
		env.IntentVerificationMode != "" ||
		req.ExpectedUse != "" ||
		schemaVersion != 1 {
		dedupParts = append(dedupParts,
			req.PlannedCalls,
			req.ExpectedTools,
			req.ExpectedEgress,
			req.RequiredCredentials,
			env.IntentVerificationMode,
			req.ExpectedUse,
			schemaVersion,
		)
	}
	dedupParts = append(dedupParts, lifetime)
	if req.ChainExtractionMode != "" {
		dedupParts = append(dedupParts, "chain_extraction_mode", req.ChainExtractionMode)
	}
	taskDedupKey := buildDedupKey(dedupParts...)
	if cached, ok := h.contentDedup.Get(taskDedupKey); ok {
		resp := cached.(map[string]any)
		writeJSON(w, http.StatusCreated, resp)
		return
	}

	expiresIn := req.ExpiresInSeconds
	if expiresIn <= 0 {
		expiresIn = h.cfg.Task.DefaultExpirySeconds
	}

	// All tasks start as pending_approval — no policy-based auto-activation.
	task := &store.Task{
		ID:                     uuid.New().String(),
		UserID:                 agent.UserID,
		AgentID:                agent.ID,
		Purpose:                req.Purpose,
		Status:                 "pending_approval",
		Lifetime:               lifetime,
		AuthorizedActions:      req.AuthorizedActions,
		PlannedCalls:           req.PlannedCalls,
		IntentVerificationMode: env.IntentVerificationMode,
		ChainExtractionMode:    req.ChainExtractionMode,
		ExpectedUse:            req.ExpectedUse,
		SchemaVersion:          schemaVersion,
		ExpiresInSeconds:       expiresIn,
	}
	if req.CallbackURL != "" {
		task.CallbackURL = &req.CallbackURL
	}
	toolsRaw, egressRaw, credsRaw, err := runtimetasks.EnvelopeToRawColumns(env)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "could not encode task envelope")
		return
	}
	task.ExpectedTools = toolsRaw
	task.ExpectedEgress = egressRaw
	task.RequiredCredentials = credsRaw

	// Run risk assessment (non-blocking — errors are logged, not propagated).
	// Both schema versions route through the LLM assessor: v1 carries
	// AuthorizedActions/PlannedCalls, v2 carries the runtime envelope.
	// The deterministic envelope-shape policy serves as a floor for v2
	// tasks so structural amplifiers (wildcard hosts, regex matchers,
	// intent verification off) are never under-graded by the LLM.
	if h.assessor != nil {
		var assessment *taskrisk.RiskAssessment
		if len(req.AuthorizedActions) > 0 || hasV2Fields {
			llmReq := taskrisk.AssessRequest{
				Purpose:           req.Purpose,
				AuthorizedActions: req.AuthorizedActions,
				PlannedCalls:      req.PlannedCalls,
				AgentName:         agent.Name,
				UserID:            agent.UserID,
			}
			if hasV2Fields {
				llmReq.ExpectedTools = env.ExpectedTools
				llmReq.ExpectedEgress = env.ExpectedEgress
				llmReq.RequiredCredentials = env.RequiredCredentials
				llmReq.IntentVerificationMode = env.IntentVerificationMode
				llmReq.ExpectedUse = env.ExpectedUse
			}
			llmAssessment, err := h.assessor.Assess(ctx, llmReq)
			if err != nil {
				h.logger.WarnContext(ctx, "task risk assessment failed", "error", err)
			}
			assessment = llmAssessment
		}
		if hasV2Fields {
			envelopeAssessment := runtimepolicy.AssessTaskEnvelope(req.Purpose, env)
			assessment = mergeRiskAssessments(assessment, envelopeAssessment)
		}
		if assessment != nil {
			task.RiskLevel = assessment.RiskLevel
			task.RiskDetails = taskrisk.MarshalAssessment(assessment)
		}
	} else if hasV2Fields {
		assessment := runtimepolicy.AssessTaskEnvelope(req.Purpose, env)
		task.RiskLevel = assessment.RiskLevel
		task.RiskDetails = taskrisk.MarshalAssessment(assessment)
	}

	// Check for group chat approval via LLM analysis of recent messages.
	// Only auto-approve session tasks with low/medium risk and no hardcoded approval actions.
	// Standing tasks always require explicit user approval.
	// The agent must be paired to a group chat and the user must have opted in.
	preApproved := false
	groupChatID := ""
	if h.agentPairer != nil {
		groupChatID, _ = h.agentPairer.AgentGroupChatID(ctx, agent.ID)
	}
	autoApprovalEnabled := false
	autoApprovalNotify := true // on by default
	if groupChatID != "" {
		if tg, err := h.st.GetTelegramGroup(ctx, agent.UserID, groupChatID); err == nil {
			autoApprovalEnabled = tg.AutoApprovalEnabled
			autoApprovalNotify = tg.AutoApprovalNotify
		}
	}
	if autoApprovalEnabled && groupChatID != "" && h.msgBuffer != nil && h.llmHealth != nil &&
		task.Lifetime != "standing" &&
		task.RiskLevel != "high" && task.RiskLevel != "critical" {
		hasHardcoded := false
		for _, a := range req.AuthorizedActions {
			if RequiresHardcodedApproval(a.Service, a.Action) {
				hasHardcoded = true
				break
			}
		}
		if !hasHardcoded {
			messages := h.msgBuffer.Messages(groupChatID)
			if len(messages) > 0 {
				var actionStrs []string
				for _, a := range req.AuthorizedActions {
					actionStrs = append(actionStrs, a.Service+":"+a.Action)
				}
				// Resolve the user's authorized Telegram user_id from their
				// notification config. The DM chat_id captured at pairing
				// time IS the user's Telegram user_id (Telegram DMs use
				// user-to-bot, where chat_id == sender id). Anyone else's
				// messages are filtered out by CheckApproval before the
				// LLM sees them — closing the display-name spoofing surface.
				var authorizedSenderIDs []string
				if cs, ok := h.notifier.(notify.TelegramConfigStore); ok {
					if _, chatID, err := cs.TelegramConfig(ctx, agent.UserID); err == nil && chatID != "" {
						authorizedSenderIDs = []string{chatID}
					}
				}
				result, err := intent.CheckApproval(ctx, h.llmHealth, intent.ApprovalCheckRequest{
					Messages:            messages,
					AuthorizedSenderIDs: authorizedSenderIDs,
					TaskPurpose:         req.Purpose,
					TaskActions:         actionStrs,
					AgentName:           agent.Name,
				})
				if err != nil {
					h.logger.WarnContext(ctx, "group chat approval check failed", "err", err, "user_id", agent.UserID)
				} else if result != nil && result.Approved {
					preApproved = true
					task.Status = "active"
					now := time.Now().UTC()
					task.ApprovedAt = &now
					expiresAt := now.Add(time.Duration(task.ExpiresInSeconds) * time.Second)
					task.ExpiresAt = &expiresAt
					task.ApprovalSource = "telegram_group"
					rationale, _ := json.Marshal(map[string]any{
						"explanation": result.Explanation,
						"confidence":  result.Confidence,
						"model":       result.Model,
						"latency_ms":  result.LatencyMS,
					})
					task.ApprovalRationale = rationale
					h.logger.InfoContext(ctx, "task auto-approved via group chat LLM check",
						"task_id", task.ID, "confidence", result.Confidence,
						"explanation", result.Explanation, "model", result.Model,
						"latency_ms", result.LatencyMS)
				}
			}
		}
	}

	if err := h.st.CreateTask(ctx, task); err != nil {
		h.logger.WarnContext(ctx, "create task failed", "err", err)
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not create task")
		return
	}
	if !preApproved {
		if err := h.createCanonicalTaskApproval(ctx, task, "task_create"); err != nil {
			h.logger.ErrorContext(ctx, "failed to create canonical task approval", "task_id", task.ID, "err", err)
		}
	}

	if preApproved {
		// Send confirmation DM (if notifications enabled).
		if h.notifier != nil && autoApprovalNotify {
			text := fmt.Sprintf("✅ <b>Task auto-approved</b> (group chat observation)\n\n"+
				"<b>Agent:</b> %s\n<b>Purpose:</b> %s",
				agent.Name, req.Purpose)
			if task.RiskLevel != "" {
				text += fmt.Sprintf("\n<b>Risk:</b> %s", task.RiskLevel)
			}
			_ = h.notifier.SendAlert(ctx, agent.UserID, text)
		}

		// Deliver callback to agent if configured.
		if task.CallbackURL != nil && *task.CallbackURL != "" {
			cbKey, _ := h.st.GetAgentCallbackSecret(ctx, task.AgentID)
			h.dispatchCallback(*task.CallbackURL, &callback.Payload{
				Type:   "task",
				TaskID: task.ID,
				Status: "approved",
			}, cbKey)
		}

		h.publishTasksAndQueue(agent.UserID)

		resp := map[string]any{
			"task_id":         task.ID,
			"status":          "active",
			"message":         "Task auto-approved via Telegram group pre-approval.",
			"approval_source": "telegram_group_observation",
		}
		if task.ExpiresAt != nil {
			resp["expires_at"] = task.ExpiresAt.Format(time.RFC3339)
		}
		h.contentDedup.Put(taskDedupKey, resp)
		writeJSON(w, http.StatusCreated, resp)
		return
	}

	// Send notification.
	if h.notifier != nil {
		approveURL := fmt.Sprintf("%s/dashboard/tasks?action=approve&task_id=%s", h.baseURL, task.ID)
		denyURL := fmt.Sprintf("%s/dashboard/tasks?action=deny&task_id=%s", h.baseURL, task.ID)
		expiresInStr := fmt.Sprintf("%d minutes", expiresIn/60)
		if lifetime == "standing" {
			expiresInStr = "standing (no expiry)"
		}

		if msgID, err := h.notifier.SendTaskApprovalRequest(ctx, notify.TaskApprovalRequest{
			TaskID:       task.ID,
			UserID:       agent.UserID,
			AgentName:    agent.Name,
			Purpose:      req.Purpose,
			Actions:      req.AuthorizedActions,
			PlannedCalls: req.PlannedCalls,
			ScopeSummary: taskScopeSummary(req.AuthorizedActions, req.ExpectedTools, req.ExpectedEgress),
			RiskLevel:    task.RiskLevel,
			ApproveURL:   approveURL,
			DenyURL:      denyURL,
			ExpiresIn:    expiresInStr,
		}); err != nil {
			h.logger.WarnContext(ctx, "failed to send task approval notification", "task_id", task.ID, "err", err)
		} else if msgID != "" {
			_ = h.st.SaveNotificationMessage(ctx, "task", task.ID, "telegram", msgID)
		}
	}

	h.publishTasksAndQueue(agent.UserID)

	// If wait=true, long-poll until the task is approved or denied.
	if r.URL.Query().Get("wait") == "true" && h.eventHub != nil {
		resolved := h.waitForTaskResolution(ctx, task.ID, agent.UserID, longPollDeadline(r))
		if r.Context().Err() != nil {
			return
		}
		sanitizeTaskForResponse(resolved)
		writeJSON(w, http.StatusCreated, resolved)
		return
	}

	resp := map[string]any{
		"task_id": task.ID,
		"status":  "pending_approval",
		"message": "Task approval requested. Waiting for human review.",
	}
	h.contentDedup.Put(taskDedupKey, resp)
	writeJSON(w, http.StatusCreated, resp)
}

func taskScopeSummary(actions []store.TaskAction, tools []runtimetasks.ExpectedTool, egress []runtimetasks.ExpectedEgress) []string {
	summary := make([]string, 0, len(actions)+len(tools)+len(egress))
	for _, a := range actions {
		if a.Service == "" || a.Action == "" {
			continue
		}
		mode := "auto-execute"
		if !a.AutoExecute {
			mode = "requires per-request approval"
		}
		summary = append(summary, fmt.Sprintf("%s.%s (%s)", a.Service, a.Action, mode))
	}
	for _, tool := range tools {
		name := strings.TrimSpace(tool.ToolName)
		if name == "" {
			continue
		}
		line := "tool " + name
		if why := strings.TrimSpace(tool.Why); why != "" {
			line += " — " + why
		}
		summary = append(summary, line)
	}
	for _, eg := range egress {
		host := strings.TrimSpace(eg.Host)
		if host == "" {
			continue
		}
		method := strings.TrimSpace(eg.Method)
		path := firstNonEmptyLog(eg.Path, eg.PathRegex)
		line := "egress " + host
		if method != "" || path != "" {
			line += " " + strings.TrimSpace(method+" "+path)
		}
		if why := strings.TrimSpace(eg.Why); why != "" {
			line += " — " + why
		}
		summary = append(summary, line)
	}
	return summary
}

// ── Get ───────────────────────────────────────────────────────────────────────

// Get returns task details. Agent must own the task via agent's user_id.
//
// GET /api/tasks/{id}
// Auth: agent bearer token
//
// Query params:
//
//	wait=true    – long-poll until the task leaves a pending state (or timeout)
//	timeout=N    – wait timeout in seconds (default 120, max 120)
func (h *TasksHandler) Get(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	agent := middleware.AgentFromContext(ctx)
	if agent == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}

	taskID := r.PathValue("id")
	task, err := h.st.GetTask(ctx, taskID)
	if err != nil {
		if err == store.ErrNotFound {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "task not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not get task")
		return
	}
	if task.UserID != agent.UserID {
		writeError(w, http.StatusForbidden, "FORBIDDEN", "not your task")
		return
	}

	// Long-poll: if wait=true and task is still pending, block until it
	// transitions or the timeout elapses.
	if r.URL.Query().Get("wait") == "true" && isTaskPending(task.Status) && h.eventHub != nil {
		task = h.waitForTaskResolution(ctx, taskID, agent.UserID, longPollDeadline(r))
		if r.Context().Err() != nil {
			return
		}
	}

	sanitizeTaskForResponse(task)
	writeJSON(w, http.StatusOK, task)
}

// Cost returns the LLM token usage and computed cost (in micro-USD)
// for one task, rolled up across all upstream calls made under that
// task. Authenticated by user (not agent) — the dashboard surfaces
// per-task spend; agents don't need this view.
//
// GET /api/tasks/{id}/cost
// Auth: user session
func (h *TasksHandler) Cost(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user := middleware.UserFromContext(ctx)
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}
	taskID := r.PathValue("id")
	if taskID == "" {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "missing task id")
		return
	}
	// Confirm the task belongs to this user before exposing its cost.
	// GetTaskCost itself filters by user_id, but a missing task should
	// still 404 rather than silently returning a zeroed summary.
	task, err := h.st.GetTask(ctx, taskID)
	if err != nil {
		if err == store.ErrNotFound {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "task not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not get task")
		return
	}
	if task.UserID != user.ID {
		writeError(w, http.StatusForbidden, "FORBIDDEN", "not your task")
		return
	}
	summary, err := h.st.GetTaskCost(ctx, user.ID, taskID)
	if err != nil {
		h.logger.WarnContext(ctx, "GetTaskCost failed", "task_id", taskID, "err", err.Error())
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not load task cost")
		return
	}
	writeJSON(w, http.StatusOK, summary)
}

// Start resolves a task into the active state and, when provided a runtime
// session id, binds that runtime session to the task for subsequent proxy
// attribution and biasing.
//
// POST /api/tasks/{id}/start
// Auth: agent bearer token
//
// Query params:
//
//	wait=true    – long-poll until the task leaves a pending state (or timeout)
//	timeout=N    – wait timeout in seconds (default 120, max 120)
//
// Body:
//
//	{"session_id":"<runtime session id>"}
func (h *TasksHandler) Start(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	agent := middleware.AgentFromContext(ctx)
	if agent == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}

	taskID := r.PathValue("id")
	task, err := h.st.GetTask(ctx, taskID)
	if err != nil {
		if err == store.ErrNotFound {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "task not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not get task")
		return
	}
	if task.UserID != agent.UserID || task.AgentID != agent.ID {
		writeError(w, http.StatusForbidden, "FORBIDDEN", "not your task")
		return
	}
	if r.URL.Query().Get("wait") == "true" && isTaskPending(task.Status) && h.eventHub != nil {
		task = h.waitForTaskResolution(ctx, taskID, agent.UserID, longPollDeadline(r))
		if r.Context().Err() != nil {
			return
		}
	}

	var req struct {
		SessionID        string `json:"session_id"`
		RuntimeSessionID string `json:"runtime_session_id"`
	}
	if r.Body != nil && r.ContentLength != 0 {
		if !decodeJSON(w, r, &req) {
			return
		}
	}

	if task.Status != "active" {
		sanitizeTaskForResponse(task)
		writeJSON(w, http.StatusOK, task)
		return
	}

	resp := map[string]any{
		"task_id": task.ID,
		"status":  task.Status,
	}
	sessionID := strings.TrimSpace(req.SessionID)
	if sessionID == "" {
		sessionID = strings.TrimSpace(req.RuntimeSessionID)
	}
	if sessionID == "" {
		if task.ExpiresAt != nil {
			resp["expires_at"] = task.ExpiresAt.Format(time.RFC3339)
		}
		writeJSON(w, http.StatusOK, resp)
		return
	}

	sess, err := h.st.GetRuntimeSession(ctx, sessionID)
	if err != nil {
		if err == store.ErrNotFound {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "runtime session not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not get runtime session")
		return
	}
	if sess.UserID != agent.UserID || sess.AgentID != agent.ID {
		writeError(w, http.StatusForbidden, "FORBIDDEN", "not your runtime session")
		return
	}
	if sess.RevokedAt != nil || !sess.ExpiresAt.After(time.Now().UTC()) {
		writeError(w, http.StatusGone, "RUNTIME_SESSION_EXPIRED", "runtime session is no longer active")
		return
	}
	now := time.Now().UTC()
	startedAt := now
	if existing, err := h.st.GetActiveTaskSession(ctx, task.ID, sess.ID); err == nil && existing != nil {
		startedAt = existing.StartedAt
	}
	metadata, _ := json.Marshal(map[string]any{
		"source": "task_start",
	})
	if err := h.st.UpsertActiveTaskSession(ctx, &store.ActiveTaskSession{
		TaskID:       task.ID,
		SessionID:    sess.ID,
		UserID:       sess.UserID,
		AgentID:      sess.AgentID,
		Status:       "active",
		MetadataJSON: metadata,
		StartedAt:    startedAt,
		LastSeenAt:   now,
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not bind runtime session to task")
		return
	}
	resp["session_id"] = sess.ID
	if task.ExpiresAt != nil {
		resp["expires_at"] = task.ExpiresAt.Format(time.RFC3339)
	}
	writeJSON(w, http.StatusOK, resp)
}

// isTaskPending returns true if the status represents a state that is
// waiting on user action.
func isTaskPending(status string) bool {
	return status == "pending_approval" || status == "pending_scope_expansion"
}

// waitForTaskResolution long-polls until the task leaves a pending state
// (approved/denied) or the timeout expires.
func (h *TasksHandler) waitForTaskResolution(ctx context.Context, taskID, userID string, timeout time.Duration) *store.Task {
	return events.WaitFor(ctx, h.eventHub, userID, timeout,
		[]string{"tasks"},
		func(c context.Context) (*store.Task, bool) {
			t, err := h.st.GetTask(c, taskID)
			if err != nil {
				return &store.Task{ID: taskID}, false
			}
			return t, !isTaskPending(t.Status)
		},
	)
}

// ── List ──────────────────────────────────────────────────────────────────────

// List returns pending and active tasks for the authenticated user.
//
// GET /api/tasks
// Auth: user JWT
func (h *TasksHandler) List(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user := middleware.UserFromContext(ctx)
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}

	var filter store.TaskFilter
	if r.URL.Query().Get("active_only") == "true" {
		filter.ActiveOnly = true
	}
	if v := r.URL.Query().Get("status"); v != "" {
		filter.Status = v
	}
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			filter.Limit = n
			if filter.Limit > maxListLimit {
				filter.Limit = maxListLimit
			}
		}
	}
	if v := r.URL.Query().Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			filter.Offset = n
		}
	}

	h.logger.InfoContext(ctx, "listing tasks", "active_only", filter.ActiveOnly, "status", filter.Status, "limit", filter.Limit, "offset", filter.Offset)

	tasks, total, err := h.st.ListTasks(ctx, user.ID, filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not list tasks")
		return
	}
	if tasks == nil {
		tasks = []*store.Task{}
	}
	for _, t := range tasks {
		if sanitizeTaskForResponse(t) {
			// Opportunistically mark expired tasks in the DB so the
			// background sweep doesn't have to catch them later.
			_ = h.st.UpdateTaskStatus(ctx, t.ID, "expired")
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"total": total,
		"tasks": tasks,
	})
}

// sanitizeTaskForResponse cleans up task fields before serialization:
//   - Standing tasks: nil out the sentinel expiry so it doesn't leak.
//   - Active session tasks past their expiry: report status as "expired"
//     even if the background cleanup goroutine hasn't swept them yet.
//   - Pending-scope-expansion tasks: populate PendingDerivedActions
//     so clients can render the auto-execute disposition for each
//     derived gateway scope without replicating the hardcoded-approval
//     table client-side.
func sanitizeTaskForResponse(t *store.Task) (nowExpired bool) {
	populatePendingDerivedActions(t)
	if t.Lifetime == "standing" {
		t.ExpiresAt = nil
		t.ExpiresInSeconds = 0
		return false
	}
	if t.Status == "active" && t.ExpiresAt != nil && t.ExpiresAt.Before(time.Now()) {
		t.Status = "expired"
		return true
	}
	return false
}

// populatePendingDerivedActions fills the response-only
// PendingDerivedActions field on a task in pending_scope_expansion.
// Each ExpectedTool whose tool_name parses as service:action is
// materialized into its effective TaskAction so the reviewer sees:
//   - which service / action will be granted,
//   - the per-entry why (carried into ExpansionRationale),
//   - the AutoExecute disposition: derived NEW actions always land with
//     AutoExecute=false (strict opt-in), and REPLACED actions preserve
//     the parent's AutoExecute. The reviewer relaxes per-action via
//     the dashboard's scope-overrides surface after approving.
//
// A corrupt pending row produces no derived actions — the read endpoint
// can still return the rest of the task, and buildExpansionApprovalUpdate
// will surface the corruption on approve.
func populatePendingDerivedActions(t *store.Task) {
	if t == nil {
		return
	}
	derived, err := derivedActionsFromPending(t)
	if err != nil {
		// Corrupt pending row: leave PendingDerivedActions empty so
		// the read path can still return the task, but log so an
		// operator can correlate "expansion approval prompt shows
		// nothing actionable" with a decode failure rather than
		// hunting silently. The approve handler will independently
		// 500 when buildExpansionApprovalUpdate hits the same decode.
		// Using slog.Default rather than threading a handler-scoped
		// logger keeps sanitizeTaskForResponse / List / Get callers
		// free of an extra parameter; this is a rare failure mode.
		slog.Default().Warn("derivedActionsFromPending decode failed", "task_id", t.ID, "err", err)
		return
	}
	if len(derived) == 0 {
		return
	}
	t.PendingDerivedActions = derived
}

// derivedActionsFromPending returns the TaskAction entries that the
// currently-pending expansion proposes — net-new actions plus
// replacements of existing entries' rationale. The filter is keyed on
// the CURRENT additions' ExpectedTools service:action set, not on
// whether the merged entry has a non-empty ExpansionRationale: that
// rationale persists from prior approved expansions and would
// otherwise leak into the pending diff on re-expansion.
//
// Shared by populatePendingDerivedActions (response rendering) and
// buildExpansionApprovalUpdate (persistence) so the two paths can't
// drift on "what counts as a derived action".
//
// Returns (nil, nil) when the task is not in pending_scope_expansion
// or has no PendingExpansion — both are normal cases for non-pending
// reads. Returns a non-nil error only when the pending JSON is corrupt;
// callers decide whether to fail-loud (approve path) or skip silently
// (read path).
func derivedActionsFromPending(t *store.Task) ([]store.TaskAction, error) {
	if t == nil || t.Status != "pending_scope_expansion" || t.PendingExpansion == nil {
		return nil, nil
	}
	additions, err := pendingExpansionToEnvelope(t.PendingExpansion)
	if err != nil {
		return nil, err
	}
	// Build the set of (service, action) keys that the CURRENT
	// additions actually propose. Only entries in this set should
	// surface as derived — a prior expansion's rationale on a parent
	// action would otherwise misrepresent itself as part of this
	// pending diff.
	currentKeys := make(map[string]struct{}, len(additions.ExpectedTools))
	for _, tool := range additions.ExpectedTools {
		service, action, ok := parseToolNameAsServiceAction(tool.ToolName)
		if !ok {
			continue
		}
		currentKeys[authorizedActionKey(service, action)] = struct{}{}
	}
	if len(currentKeys) == 0 {
		return nil, nil
	}
	merged := mergeAuthorizedActionsFromExpansion(t.AuthorizedActions, t.IntentVerificationMode, additions.ExpectedTools)
	var derived []store.TaskAction
	seen := make(map[string]struct{}, len(merged))
	for _, a := range merged {
		key := authorizedActionKey(a.Service, a.Action)
		if _, proposed := currentKeys[key]; !proposed {
			continue
		}
		derived = append(derived, a)
		seen[key] = struct{}{}
	}
	// Synthesize WildcardCovered entries for additions whose specific
	// service:action is covered by a same-service wildcard on the
	// parent. mergeAuthorizedActionsFromExpansion drops these from
	// the merged set (the wildcard already authorizes them, so
	// persistence stays clean), but the response-only projection
	// SHOULD surface them so API consumers see the same scope-broaden
	// signal the dashboard/Telegram/TUI surfaces compute from
	// parent.authorized_actions. The wildcard's AutoExecute /
	// Verification carry through, and ExpansionRationale comes from
	// the addition's per-entry Why.
	wildcardByService := make(map[string]store.TaskAction)
	for _, a := range t.AuthorizedActions {
		if a.Action == "*" {
			wildcardByService[strings.ToLower(strings.TrimSpace(a.Service))] = a
		}
	}
	for _, tool := range additions.ExpectedTools {
		service, action, ok := parseToolNameAsServiceAction(tool.ToolName)
		if !ok {
			continue
		}
		key := authorizedActionKey(service, action)
		if _, already := seen[key]; already {
			continue
		}
		wildcard, covered := wildcardByService[strings.ToLower(strings.TrimSpace(service))]
		if !covered {
			continue
		}
		derived = append(derived, store.TaskAction{
			Service:            service,
			Action:             action,
			AutoExecute:        wildcard.AutoExecute,
			Verification:       wildcard.Verification,
			ExpansionRationale: strings.TrimSpace(tool.Why),
			WildcardCovered:    true,
		})
	}
	return derived, nil
}

// ── Approve ───────────────────────────────────────────────────────────────────

// Approve sets the task to active and starts its expiry timer.
//
// POST /api/tasks/{id}/approve
// Auth: user JWT
func (h *TasksHandler) Approve(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user := middleware.UserFromContext(ctx)
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}

	taskID := r.PathValue("id")
	task, err := h.st.GetTask(ctx, taskID)
	if err != nil {
		if err == store.ErrNotFound {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "task not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not get task")
		return
	}
	if task.UserID != user.ID {
		writeError(w, http.StatusForbidden, "FORBIDDEN", "not your task")
		return
	}
	if task.Status != "pending_approval" {
		if h.respondActiveCredentialApprovalRetry(ctx, w, user.ID, task) {
			return
		}
		writeError(w, http.StatusConflict, "INVALID_STATE", "task is not pending approval")
		return
	}
	if isInlineChatPending(task) {
		// Chat-bound pending task. Dashboard cannot resolve it — the
		// llmproxy cache hold is the in-process anchor, and approving
		// here would flip the DB row without telling the model. Refuse
		// with a clear code so the dashboard can render the pointer
		// back to chat.
		writeError(w, http.StatusConflict, "INLINE_CHAT_BOUND", "approve in the agent chat surface — reply 'approve' or 'deny' to the running session")
		return
	}

	// Optionally parse per-scope overrides from the request body. Each override
	// targets one authorized_action by (service, action) and may set verification
	// and/or auto_execute. Missing fields leave the scope's current value intact.
	// An empty body is allowed (no overrides); a malformed body is a client error.
	var overrides []scopeOverride
	if r.Body != nil {
		var body struct {
			Scopes []scopeOverride `json:"scopes"`
		}
		if decErr := json.NewDecoder(r.Body).Decode(&body); decErr != nil && !errors.Is(decErr, io.EOF) {
			writeError(w, http.StatusBadRequest, "INVALID_BODY", "could not parse body")
			return
		}
		overrides = body.Scopes
	}
	validModes := map[string]bool{"strict": true, "lenient": true, "off": true}
	for _, o := range overrides {
		if o.Verification != "" && !validModes[o.Verification] {
			writeError(w, http.StatusBadRequest, "INVALID_VERIFICATION_MODE",
				"verification must be one of: strict, lenient, off")
			return
		}
	}

	// Apply overrides to the task's authorized actions.
	actions := applyScopeOverrides(task.AuthorizedActions, overrides)
	requiredCredentials, err := taskRequiredCredentials(task)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "could not parse required_credentials")
		return
	}
	if err := h.validateTaskRequiredCredentials(ctx, task, requiredCredentials); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_CREDENTIAL_REQUEST", err.Error())
		return
	}

	expiresAt := taskApprovedExpiresAt(task)

	credentialPlaceholders, err := h.ensureTaskCredentialPlaceholders(ctx, task, requiredCredentials, expiresAt)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not mint task credential placeholders")
		return
	}

	won, err := h.st.UpdateTaskApprovedFrom(ctx, taskID, "pending_approval", expiresAt, actions)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not approve task")
		return
	}
	if !won {
		// Concurrent approve/deny race or already-resolved task — refuse to
		// repeat the side effects below (callback, audit).
		writeError(w, http.StatusConflict, "INVALID_STATE", "task is no longer pending approval")
		return
	}
	h.resolveCanonicalTaskApproval(ctx, task, "task_create", taskApprovalResolution(task), "approved")

	// Deliver callback if set.
	if task.CallbackURL != nil && *task.CallbackURL != "" {
		cbKey, _ := h.st.GetAgentCallbackSecret(ctx, task.AgentID)
		h.dispatchCallback(*task.CallbackURL, &callback.Payload{
			Type:   "task",
			TaskID: taskID,
			Status: "approved",
		}, cbKey)
	}

	h.publishTasksAndQueue(user.ID)

	resp := map[string]any{
		"task_id": taskID,
		"status":  "active",
	}
	if task.Lifetime != "standing" {
		resp["expires_at"] = expiresAt.Format(time.RFC3339)
	}
	if len(credentialPlaceholders) > 0 {
		resp["credential_placeholders"] = credentialPlaceholders
	}
	writeJSON(w, http.StatusOK, resp)
}

func (h *TasksHandler) respondActiveCredentialApprovalRetry(ctx context.Context, w http.ResponseWriter, userID string, task *store.Task) bool {
	if task == nil || task.Status != "active" {
		return false
	}
	requiredCredentials, err := taskRequiredCredentials(task)
	if err != nil || len(requiredCredentials) == 0 {
		return false
	}
	if err := h.validateTaskRequiredCredentials(ctx, task, requiredCredentials); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_CREDENTIAL_REQUEST", err.Error())
		return true
	}
	credentialPlaceholders, err := h.ensureTaskCredentialPlaceholders(ctx, task, requiredCredentials, taskCredentialExpiry(task))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not mint task credential placeholders")
		return true
	}
	h.resolveCanonicalTaskApproval(ctx, task, "task_create", taskApprovalResolution(task), "approved")
	h.publishTasksAndQueue(userID)

	resp := map[string]any{
		"task_id": task.ID,
		"status":  "active",
	}
	if task.ExpiresAt != nil && task.Lifetime != "standing" {
		resp["expires_at"] = task.ExpiresAt.UTC().Format(time.RFC3339)
	}
	if len(credentialPlaceholders) > 0 {
		resp["credential_placeholders"] = credentialPlaceholders
	}
	writeJSON(w, http.StatusOK, resp)
	return true
}

// scopeOverride targets one authorized_action by (service, action) and
// optionally sets verification and/or auto_execute.
type scopeOverride struct {
	Service      string `json:"service"`
	Action       string `json:"action"`
	Verification string `json:"verification,omitempty"`
	AutoExecute  *bool  `json:"auto_execute,omitempty"`
}

// applyScopeOverrides returns a copy of actions with verification and
// auto_execute applied from any override whose (service, action) matches.
// Non-matching overrides are ignored.
func applyScopeOverrides(actions []store.TaskAction, overrides []scopeOverride) []store.TaskAction {
	if len(overrides) == 0 {
		return actions
	}
	out := make([]store.TaskAction, len(actions))
	copy(out, actions)
	for i := range out {
		for _, o := range overrides {
			if o.Service != out[i].Service || o.Action != out[i].Action {
				continue
			}
			if o.Verification != "" {
				out[i].Verification = o.Verification
			}
			if o.AutoExecute != nil {
				out[i].AutoExecute = *o.AutoExecute
			}
		}
	}
	return out
}

func mergeRiskAssessments(primary, secondary *taskrisk.RiskAssessment) *taskrisk.RiskAssessment {
	if primary == nil {
		return secondary
	}
	if secondary == nil {
		return primary
	}
	out := *primary
	out.RiskLevel = highestRiskLevel(primary.RiskLevel, secondary.RiskLevel)
	out.Factors = append(append([]string{}, primary.Factors...), secondary.Factors...)
	out.Conflicts = append(append([]taskrisk.ConflictDetail{}, primary.Conflicts...), secondary.Conflicts...)
	if secondary.Explanation != "" && highestRiskLevel(primary.RiskLevel, secondary.RiskLevel) == secondary.RiskLevel {
		out.Explanation = secondary.Explanation
	}
	if out.Model == "" {
		out.Model = secondary.Model
	}
	if out.LatencyMS == 0 {
		out.LatencyMS = secondary.LatencyMS
	}
	return &out
}

func taskRequiredCredentials(task *store.Task) ([]runtimetasks.RequiredCredential, error) {
	if task == nil || len(task.RequiredCredentials) == 0 {
		return nil, nil
	}
	var required []runtimetasks.RequiredCredential
	if err := json.Unmarshal(task.RequiredCredentials, &required); err != nil {
		return nil, err
	}
	return required, nil
}

func (h *TasksHandler) validateTaskRequiredCredentials(ctx context.Context, task *store.Task, required []runtimetasks.RequiredCredential) error {
	if len(required) == 0 {
		return nil
	}
	if h.vault == nil {
		return fmt.Errorf("vault is not configured")
	}
	for i, cred := range required {
		vaultItemID := credentialVaultItemID(cred)
		if vaultItemID == "" {
			return fmt.Errorf("required_credentials[%d] must include vault_item_id or vault_item_handle", i)
		}
		storageKey, err := h.taskVaultItemStorageKey(ctx, task, vaultItemID)
		if err != nil {
			return err
		}
		if _, err := h.vault.Get(ctx, task.UserID, storageKey); err != nil {
			if errors.Is(err, vault.ErrNotFound) {
				return h.vaultItemNotAvailableError(ctx, task.UserID, vaultItemID)
			}
			return fmt.Errorf("could not verify vault item %q", vaultItemID)
		}
	}
	return nil
}

// vaultItemNotAvailableError formats a credential-validation failure with a
// recovery hint. The agent's first instinct on "not available" is to tell the
// user the credential is missing; this nudges it to discover the correct
// handle instead. We surface up to a few candidate vault item IDs sharing the
// requested service prefix (e.g. `github` → `github:ericlevine`) so the agent
// can retry without a separate round-trip. Listing failures are silent — the
// hint to GET /control/vault/items always fires.
func (h *TasksHandler) vaultItemNotAvailableError(ctx context.Context, userID, vaultItemID string) error {
	candidates := h.candidateVaultItemIDs(ctx, userID, vaultItemID)
	hint := "list GET /control/vault/items to find the correct handle (vault items may be account-aliased, e.g. `github:account`) and retry the task"
	if len(candidates) > 0 {
		hint = fmt.Sprintf("did you mean %s? Vault items may be account-aliased; list GET /control/vault/items for the full set and retry the task", strings.Join(quoteStrings(candidates), ", "))
	}
	return fmt.Errorf("vault item %q is not available — %s", vaultItemID, hint)
}

// candidateVaultItemIDs returns up to 3 public vault item IDs whose service
// prefix matches the requested handle. We source these from
// listVaultItemIDs — the same list /control/vault/items exposes — so we
// never suggest internal storage keys (agent-scoped LLM keys, hidden
// adapter backing keys) that the agent couldn't actually request. The match
// is conservative: exact prefix followed by `:` or `.` so that asking for
// `github` surfaces `github:ericlevine` but not unrelated entries. Returns
// nil on any error or when the vault is unconfigured.
func (h *TasksHandler) candidateVaultItemIDs(ctx context.Context, userID, vaultItemID string) []string {
	prefix := strings.TrimSpace(vaultItemID)
	if prefix == "" {
		return nil
	}
	ids, err := listVaultItemIDs(ctx, h.st, h.vault, h.adapterReg, userID)
	if err != nil {
		return nil
	}
	out := make([]string, 0, 3)
	for _, id := range ids {
		if id == vaultItemID {
			continue
		}
		if !strings.HasPrefix(id, prefix) {
			continue
		}
		rest := id[len(prefix):]
		if rest == "" {
			continue
		}
		if rest[0] != ':' && rest[0] != '.' {
			continue
		}
		out = append(out, id)
		if len(out) >= 3 {
			break
		}
	}
	return out
}

func quoteStrings(s []string) []string {
	out := make([]string, len(s))
	for i, v := range s {
		out[i] = fmt.Sprintf("%q", v)
	}
	return out
}

func (h *TasksHandler) taskVaultItemStorageKey(ctx context.Context, task *store.Task, vaultItemID string) (string, error) {
	if task == nil {
		return "", fmt.Errorf("task is required")
	}
	vaultItemID = strings.TrimSpace(vaultItemID)
	if vaultItemID == "" {
		return "", fmt.Errorf("vault item id is required")
	}

	if agentID, _, ok := parseAgentScopedLLMVaultItemID(vaultItemID); ok {
		if agentID != task.AgentID {
			return "", fmt.Errorf("vault item %q is scoped to another agent", vaultItemID)
		}
		return vaultStorageKeyForItemID(vaultItemID), nil
	}
	if isUserScopedLLMVaultItemID(vaultItemID) {
		return vaultStorageKeyForItemID(vaultItemID), nil
	}
	if _, _, ok := parseAgentScopedLLMKey(vaultItemID); ok {
		return "", fmt.Errorf("vault item %q is not available; use a vault item id, not a storage key", vaultItemID)
	}
	if llmProviderFromVaultKey(vaultItemID) != "" {
		return "", fmt.Errorf("vault item %q is not available; use the llm provider vault item id", vaultItemID)
	}

	if h.vaultStorageKeyIsHiddenBackingKey(ctx, task.UserID, vaultItemID) {
		return "", fmt.Errorf("vault item %q is not available; request the service-specific vault item id", vaultItemID)
	}

	if h.adapterReg != nil {
		serviceID, alias := splitServiceScopedVaultItemID(vaultItemID)
		if serviceID != "" {
			if _, ok := h.adapterReg.GetForUser(ctx, serviceID, task.UserID); ok {
				return h.adapterReg.VaultKeyWithAliasForUser(serviceID, alias, task.UserID), nil
			}
		}
	}

	return vaultItemID, nil
}

func (h *TasksHandler) vaultStorageKeyIsHiddenBackingKey(ctx context.Context, userID, storageKey string) bool {
	if storageKey == "" || h.adapterReg == nil {
		return false
	}
	if metas, err := h.st.ListServiceMetas(ctx, userID); err == nil {
		for _, binding := range vaultBindingsForVaultKey(ctx, h.adapterReg, userID, storageKey, metas) {
			if connectedVaultItemID(binding) != storageKey {
				return true
			}
		}
	}
	for _, adapter := range h.adapterReg.All() {
		if adapter == nil {
			continue
		}
		serviceID := adapter.ServiceID()
		if serviceID == "" || serviceID == storageKey {
			continue
		}
		if h.adapterReg.VaultKeyForUser(serviceID, userID) == storageKey {
			return true
		}
	}
	return false
}

func parseAgentScopedLLMVaultItemID(itemID string) (agentID, provider string, ok bool) {
	parts := strings.Split(strings.TrimSpace(itemID), ":")
	if len(parts) != 4 || parts[0] != "llm" || parts[2] != "agent" || parts[3] == "" {
		return "", "", false
	}
	provider = llmProviderFromVaultKey(parts[1])
	if provider == "" {
		return "", "", false
	}
	return parts[3], provider, true
}

func (h *TasksHandler) mintTaskCredentialPlaceholders(ctx context.Context, task *store.Task, required []runtimetasks.RequiredCredential, expiresAt time.Time) ([]*store.RuntimePlaceholder, error) {
	if len(required) == 0 {
		return nil, nil
	}
	out := make([]*store.RuntimePlaceholder, 0, len(required))
	for _, cred := range required {
		vaultItemID := credentialVaultItemID(cred)
		storageKey, err := h.taskVaultItemStorageKey(ctx, task, vaultItemID)
		if err != nil {
			return nil, err
		}
		entry, err := h.mintTaskCredentialPlaceholder(ctx, task, vaultItemID, storageKey, cred.Why, expiresAt)
		if err != nil {
			return nil, err
		}
		out = append(out, entry)
	}
	return out, nil
}

func (h *TasksHandler) ensureTaskCredentialPlaceholders(ctx context.Context, task *store.Task, required []runtimetasks.RequiredCredential, expiresAt time.Time) ([]*store.RuntimePlaceholder, error) {
	if len(required) == 0 {
		return nil, nil
	}
	existing, err := h.st.ListRuntimePlaceholders(ctx, task.UserID)
	if err != nil {
		return nil, err
	}
	byVaultItem := make(map[string][]*store.RuntimePlaceholder)
	now := time.Now().UTC()
	for _, entry := range existing {
		if entry == nil || entry.TaskID != task.ID || entry.AgentID != task.AgentID {
			continue
		}
		if _, ok := llmproxy.ValidateRuntimePlaceholderAccess(ctx, h.st, entry, task.UserID, task.AgentID, now); !ok {
			continue
		}
		byVaultItem[entry.VaultItemID] = append(byVaultItem[entry.VaultItemID], entry)
	}

	out := make([]*store.RuntimePlaceholder, 0, len(required))
	for _, cred := range required {
		vaultItemID := credentialVaultItemID(cred)
		if entries := byVaultItem[vaultItemID]; len(entries) > 0 {
			out = append(out, entries[0])
			byVaultItem[vaultItemID] = entries[1:]
			continue
		}
		storageKey, err := h.taskVaultItemStorageKey(ctx, task, vaultItemID)
		if err != nil {
			return nil, err
		}
		entry, err := h.mintTaskCredentialPlaceholder(ctx, task, vaultItemID, storageKey, cred.Why, expiresAt)
		if err != nil {
			return nil, err
		}
		out = append(out, entry)
	}
	return out, nil
}

func (h *TasksHandler) mintTaskCredentialPlaceholder(ctx context.Context, task *store.Task, vaultItemID, storageKey, expectedUse string, expiresAt time.Time) (*store.RuntimePlaceholder, error) {
	auth := &store.CredentialAuthorization{
		ID:            uuid.New().String(),
		UserID:        task.UserID,
		AgentID:       task.AgentID,
		Scope:         "session",
		CredentialRef: storageKey,
		Service:       vaultItemID,
		Host:          "",
		HeaderName:    "authorization",
		Scheme:        "bearer",
		Status:        "active",
		MetadataJSON: mustJSON(map[string]any{
			"source":        "task_required_credentials",
			"scope":         "task",
			"task_id":       task.ID,
			"vault_item_id": vaultItemID,
			"expected_use":  expectedUse,
		}),
		ExpiresAt: &expiresAt,
	}
	if err := h.st.CreateCredentialAuthorization(ctx, auth); err != nil {
		return nil, err
	}
	placeholder, err := runtimeautovault.GeneratePlaceholder(runtimeautovault.PlaceholderPrefix(vaultItemID))
	if err != nil {
		return nil, err
	}
	entry := &store.RuntimePlaceholder{
		Placeholder:       placeholder,
		UserID:            task.UserID,
		AgentID:           task.AgentID,
		ServiceID:         vaultItemID,
		VaultItemID:       vaultItemID,
		CredentialGrantID: auth.ID,
		TaskID:            task.ID,
		ExpiresAt:         &expiresAt,
	}
	if err := h.st.CreateRuntimePlaceholder(ctx, entry); err != nil {
		return nil, err
	}
	return entry, nil
}

func taskCredentialExpiry(task *store.Task) time.Time {
	if task != nil && task.ExpiresAt != nil {
		return task.ExpiresAt.UTC()
	}
	return time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
}

func credentialVaultItemID(cred runtimetasks.RequiredCredential) string {
	if id := strings.TrimSpace(cred.VaultItemID); id != "" {
		return id
	}
	return strings.TrimSpace(cred.VaultItemHandle)
}

func (h *TasksHandler) vaultStorageKeyForTaskItem(ctx context.Context, userID, vaultItemID string) string {
	return vaultStorageKeyForItemIDForUser(ctx, h.adapterReg, userID, vaultItemID)
}

func highestRiskLevel(a, b string) string {
	order := map[string]int{
		"":         -1,
		"unknown":  0,
		"low":      1,
		"medium":   2,
		"high":     3,
		"critical": 4,
	}
	if order[b] > order[a] {
		return b
	}
	return a
}

// ── UpdateScope ───────────────────────────────────────────────────────────────

// UpdateScope applies per-scope overrides (verification and/or auto_execute)
// to an active task after approval. This is the post-approval editing path.
//
// PATCH /api/tasks/{id}/scope
// Body: {"scopes": [{"service":"gmail","action":"send","verification":"lenient","auto_execute":false}]}
// Auth: user JWT
func (h *TasksHandler) UpdateScope(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user := middleware.UserFromContext(ctx)
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}

	taskID := r.PathValue("id")
	task, err := h.st.GetTask(ctx, taskID)
	if err != nil {
		if err == store.ErrNotFound {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "task not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not get task")
		return
	}
	if task.UserID != user.ID {
		writeError(w, http.StatusForbidden, "FORBIDDEN", "not your task")
		return
	}
	if task.Status != "active" {
		writeError(w, http.StatusConflict, "INVALID_STATE", "task is not active")
		return
	}

	var body struct {
		Scopes []scopeOverride `json:"scopes"`
	}
	if decErr := json.NewDecoder(r.Body).Decode(&body); decErr != nil {
		writeError(w, http.StatusBadRequest, "INVALID_BODY", "could not parse body")
		return
	}
	validModes := map[string]bool{"strict": true, "lenient": true, "off": true}
	for _, o := range body.Scopes {
		if o.Verification != "" && !validModes[o.Verification] {
			writeError(w, http.StatusBadRequest, "INVALID_VERIFICATION_MODE",
				"verification must be one of: strict, lenient, off")
			return
		}
	}

	actions := applyScopeOverrides(task.AuthorizedActions, body.Scopes)
	if err := h.st.UpdateTaskAuthorizedActions(ctx, taskID, actions); err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not update scope")
		return
	}

	h.publishTasksAndQueue(user.ID)
	writeJSON(w, http.StatusOK, map[string]any{"task_id": taskID, "scopes": actions})
}

// ── Deny ──────────────────────────────────────────────────────────────────────

// Deny rejects a pending task.
//
// POST /api/tasks/{id}/deny
// Auth: user JWT
func (h *TasksHandler) Deny(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user := middleware.UserFromContext(ctx)
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}

	taskID := r.PathValue("id")
	task, err := h.st.GetTask(ctx, taskID)
	if err != nil {
		if err == store.ErrNotFound {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "task not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not get task")
		return
	}
	if task.UserID != user.ID {
		writeError(w, http.StatusForbidden, "FORBIDDEN", "not your task")
		return
	}
	if task.Status != "pending_approval" && task.Status != "pending_scope_expansion" {
		writeError(w, http.StatusConflict, "INVALID_STATE", "task is not pending approval or scope expansion")
		return
	}
	// Deny is intentionally permitted on chat-bound pending rows so
	// users can dismiss a task that's no longer actionable in the
	// conversation (e.g. the agent moved on, the model lost the
	// thread). The chat-side resolve path detects an already-terminal
	// task on a later "approve" reply and renders an explanatory
	// message to the model. Approve, by contrast, remains chat-only
	// because it grants scope+credentials that need the in-flight
	// cache hold to land coherently.
	//
	// For pending_scope_expansion specifically, we route through
	// ResolveTaskPendingExpansion so the pending_expansion_json
	// column is cleared in the SAME CAS as the status flip — leaving
	// a denied task with stale pending JSON would violate the
	// invariant the store docstring guarantees (only
	// pending_scope_expansion rows carry pending_expansion_json).
	// ExpandDeny routes to active/expired; this caller forces "denied"
	// which is treated as terminal by every downstream sweep, so the
	// chain-facts/callback path below still fires.
	won, err := h.denyTaskInState(ctx, taskID, task.Status)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not deny task")
		return
	}
	if !won {
		// Concurrent approve/deny race or already-resolved task — refuse to
		// repeat side effects (callback, chain-facts cleanup, audit).
		writeError(w, http.StatusConflict, "INVALID_STATE", "task is no longer pending")
		return
	}
	h.resolveCanonicalTaskApproval(ctx, task, canonicalTaskApprovalKind(task), "deny", "denied")
	if err := h.st.DeleteChainFactsByTask(ctx, taskID); err != nil {
		h.logger.WarnContext(ctx, "chain facts cleanup failed", "err", err, "task_id", taskID)
	}

	h.publishTasksAndQueue(user.ID)

	// Deliver callback if set.
	if task.CallbackURL != nil && *task.CallbackURL != "" {
		cbKey, _ := h.st.GetAgentCallbackSecret(ctx, task.AgentID)
		h.dispatchCallback(*task.CallbackURL, &callback.Payload{
			Type:   "task",
			TaskID: taskID,
			Status: "denied",
		}, cbKey)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"task_id": taskID,
		"status":  "denied",
	})
}

// denyTaskInState terminates a pending task with the appropriate
// store primitive for its current state. Plain pending_approval rows
// go through the generic status CAS. pending_scope_expansion rows go
// through ResolveTaskPendingExpansion(Denied) so pending_expansion_json
// clears in the SAME transaction as the status flip — leaving stale
// pending JSON on a denied row would violate the
// SetTaskPendingExpansion docstring's invariant. Returns (won, err)
// in both shapes for caller uniformity.
func (h *TasksHandler) denyTaskInState(ctx context.Context, taskID, fromStatus string) (bool, error) {
	if fromStatus == "pending_scope_expansion" {
		return h.st.ResolveTaskPendingExpansion(ctx, taskID, store.ResolveExpansionStatusDenied)
	}
	return h.st.UpdateTaskStatusFrom(ctx, taskID, fromStatus, "denied")
}

// ── Complete ──────────────────────────────────────────────────────────────────

// Complete marks a task as finished.
//
// POST /api/tasks/{id}/complete
// POST /api/control/tasks/{id}/complete
// Auth: agent bearer token (dashboard) or proxy-minted caller nonce (control).
//
// Accepts tasks in either "active" or "expired" state — an expired task
// still owns chain-fact rows that should be cleaned up on the agent's
// wrap-up call. The status transition is CAS-guarded by
// UpdateTaskStatusFrom so a parallel expand / revoke / expiration sweep
// between the preflight read and the write cannot be silently
// overwritten. Ownership is pinned to both the task's user AND its
// agent: the dashboard surface routes through the user's own session
// (so agent identity is implicit), but the control plane elevates this
// to a real per-agent boundary — another agent under the same user
// must not be able to close out (and DROP chain facts for) a task it
// does not own.
func (h *TasksHandler) Complete(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	agent := middleware.AgentFromContext(ctx)
	if agent == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}

	taskID := r.PathValue("id")
	task, err := h.st.GetTask(ctx, taskID)
	if err != nil {
		if err == store.ErrNotFound {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "task not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not get task")
		return
	}
	if task.UserID != agent.UserID || task.AgentID != agent.ID {
		writeError(w, http.StatusForbidden, "FORBIDDEN", "not your task")
		return
	}
	if task.Status != "active" && task.Status != "expired" {
		writeError(w, http.StatusConflict, "INVALID_STATE", "task is not active or expired (status: "+task.Status+")")
		return
	}

	won, err := h.st.UpdateTaskStatusFrom(ctx, taskID, task.Status, "completed")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not complete task")
		return
	}
	liveStatus := ""
	if !won {
		// Lost the CAS — re-read the task. Two cases:
		//
		//   (a) The live status is still one we accept (active or
		//       expired). The most common driver is the expiration
		//       sweeper flipping active → expired between preflight
		//       and the CAS write; that's a benign race we should
		//       absorb, not surface as a 409 — the agent's call is
		//       still valid and chain-fact cleanup still needs to
		//       run. Retry the CAS once with the live fromStatus.
		//   (b) The live status is non-completable (pending_scope_
		//       expansion, revoked, denied, already completed). The
		//       agent needs to react; 409 with the live status in
		//       the message.
		//
		// The retry is bounded to a single attempt: if it also loses,
		// fall through to the 409 branch with whatever status the
		// re-read observed. This avoids an unbounded loop if multiple
		// in-flight callers race.
		live, livErr := h.st.GetTask(ctx, taskID)
		if livErr == nil && live != nil {
			liveStatus = live.Status
			if live.Status == "active" || live.Status == "expired" {
				won, err = h.st.UpdateTaskStatusFrom(ctx, taskID, live.Status, "completed")
				if err != nil {
					writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not complete task")
					return
				}
			}
		}
	}
	if !won {
		msg := "task moved to a non-completable state"
		if liveStatus != "" {
			msg = "task moved to " + liveStatus + " before completion"
		}
		writeError(w, http.StatusConflict, "INVALID_STATE", msg)
		return
	}
	if err := h.st.DeleteChainFactsByTask(ctx, taskID); err != nil {
		h.logger.WarnContext(ctx, "chain facts cleanup failed", "err", err, "task_id", taskID)
	}

	h.publishTasksAndQueue(agent.UserID)

	writeJSON(w, http.StatusOK, map[string]any{
		"task_id": taskID,
		"status":  "completed",
	})
}

// End clears the runtime-session binding for a task without completing the task.
//
// POST /api/tasks/{id}/end
// Auth: agent bearer token
//
// Body:
//
//	{"session_id":"<runtime session id>"}
func (h *TasksHandler) End(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	agent := middleware.AgentFromContext(ctx)
	if agent == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}

	taskID := r.PathValue("id")
	task, err := h.st.GetTask(ctx, taskID)
	if err != nil {
		if err == store.ErrNotFound {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "task not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not get task")
		return
	}
	if task.UserID != agent.UserID || task.AgentID != agent.ID {
		writeError(w, http.StatusForbidden, "FORBIDDEN", "not your task")
		return
	}

	var req struct {
		SessionID        string `json:"session_id"`
		RuntimeSessionID string `json:"runtime_session_id"`
	}
	if r.Body != nil && r.ContentLength != 0 {
		if !decodeJSON(w, r, &req) {
			return
		}
	}
	sessionID := strings.TrimSpace(req.SessionID)
	if sessionID == "" {
		sessionID = strings.TrimSpace(req.RuntimeSessionID)
	}
	if sessionID == "" {
		writeDetailedError(w, http.StatusBadRequest, apiErrorDetail{
			Error:         "missing required field: runtime_session_id",
			Code:          "INVALID_REQUEST",
			MissingFields: []string{"runtime_session_id"},
			Hint:          "Provide the runtime session id to clear the task binding without completing the task.",
		})
		return
	}

	sess, err := h.st.GetRuntimeSession(ctx, sessionID)
	if err != nil {
		if err == store.ErrNotFound {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "runtime session not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not get runtime session")
		return
	}
	if sess.UserID != agent.UserID || sess.AgentID != agent.ID {
		writeError(w, http.StatusForbidden, "FORBIDDEN", "not your runtime session")
		return
	}

	if err := h.st.EndActiveTaskSession(ctx, task.ID, sess.ID, time.Now().UTC(), "ended"); err != nil {
		if err == store.ErrNotFound {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "active task session not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not end runtime task binding")
		return
	}

	h.publishTasksAndQueue(agent.UserID)
	writeJSON(w, http.StatusOK, map[string]any{
		"task_id":    task.ID,
		"status":     task.Status,
		"session_id": sess.ID,
		"binding":    "ended",
	})
}

// ── Expand ────────────────────────────────────────────────────────────────────

// expandTaskRequest mirrors the runtime envelope the agent posts at task
// creation time. The reason is the one-line summary of why these
// additions are needed — it gets surfaced verbatim in the approval
// prompt (inline + dashboard + Telegram). The body intentionally
// excludes purpose / lifetime / expires_in_seconds: an expansion
// inherits those from the parent task.
type expandTaskRequest struct {
	ExpectedTools       []runtimetasks.ExpectedTool       `json:"expected_tools,omitempty"`
	ExpectedEgress      []runtimetasks.ExpectedEgress     `json:"expected_egress,omitempty"`
	RequiredCredentials []runtimetasks.RequiredCredential `json:"required_credentials,omitempty"`
	Reason              string                            `json:"reason"`
}

// Expand requests adding tools / egress / credentials to a task's
// envelope. The body shape mirrors task creation so an agent that
// realizes mid-task it needs more capabilities can declare the same
// kind of (tool, why) entries it would have declared up front.
//
// POST /api/tasks/{id}/expand
// Auth: agent bearer token
func (h *TasksHandler) Expand(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	agent := middleware.AgentFromContext(ctx)
	if agent == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}

	taskID := r.PathValue("id")
	var req expandTaskRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	if strings.TrimSpace(req.Reason) == "" {
		writeDetailedError(w, http.StatusBadRequest, apiErrorDetail{
			Error:         "missing required field: reason",
			Code:          "INVALID_REQUEST",
			MissingFields: []string{"reason"},
			Hint:          "reason is the one-line summary surfaced verbatim in the approval prompts (Telegram, dashboard, inline). The intent verifier also reads it via expansion_rationale, so an empty value degrades every downstream surface.",
		})
		return
	}
	// Cap reason length so a runaway model can't smuggle a multi-KB
	// blob into pending_expansion_json. The reason ends up in approval
	// prompts (Telegram body, push action_summary), intent-verifier
	// context, and the canonical approval record summary, all of which
	// have practical size budgets. 512 bytes is enough for any
	// human-readable one-liner and leaves headroom on each surface.
	const maxReasonLen = 512
	if len(req.Reason) > maxReasonLen {
		writeDetailedError(w, http.StatusBadRequest, apiErrorDetail{
			Error: fmt.Sprintf("reason exceeds %d bytes", maxReasonLen),
			Code:  "INVALID_REQUEST",
			Hint:  "reason is a one-line summary surfaced verbatim in approval prompts; keep it under a few hundred characters.",
		})
		return
	}

	additions := runtimetasks.Envelope{
		ExpectedTools:       req.ExpectedTools,
		ExpectedEgress:      req.ExpectedEgress,
		RequiredCredentials: req.RequiredCredentials,
	}
	if !envelopeHasAdditions(additions) {
		writeDetailedError(w, http.StatusBadRequest, apiErrorDetail{
			Error: "expansion must declare at least one new expected_tools, expected_egress, or required_credentials entry",
			Code:  "INVALID_REQUEST",
			Hint:  "Send the same envelope shape used at task creation: a list of {tool_name, why} (and/or egress/credentials) describing the scope you now need.",
			Example: map[string]any{
				"expected_tools": []map[string]any{
					{"tool_name": "Edit", "why": "Apply fixes to the processing script"},
				},
				"reason": "Need to write a local script to process the fetched results",
			},
		})
		return
	}
	if issues := runtimepolicy.ValidateTaskEnvelopeAdditions(additions); len(issues) > 0 {
		var messages []string
		var fields []string
		for _, issue := range issues {
			messages = append(messages, issue.Field+": "+issue.Message)
			fields = append(fields, issue.Field)
		}
		writeDetailedError(w, http.StatusBadRequest, apiErrorDetail{
			Error:         strings.Join(messages, "; "),
			Code:          "INVALID_REQUEST",
			MissingFields: fields,
			Hint:          "Each expansion entry must declare a specific tool / host / credential with a valid shape and a human-readable why.",
		})
		return
	}

	task, err := h.st.GetTask(ctx, taskID)
	if err != nil {
		if err == store.ErrNotFound {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "task not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not get task")
		return
	}
	if task.UserID != agent.UserID {
		writeError(w, http.StatusForbidden, "FORBIDDEN", "not your task")
		return
	}
	if task.AgentID != agent.ID {
		// Agent-bound: an agent may not expand another agent's task, even
		// under the same user. Cross-agent expansion would silently
		// broaden an unrelated session's scope.
		writeError(w, http.StatusForbidden, "FORBIDDEN", "not your task")
		return
	}
	if task.Status != "active" && task.Status != "expired" {
		writeError(w, http.StatusConflict, "INVALID_STATE", "task must be active or expired to expand")
		return
	}
	// Standing tasks ARE expandable — the approve path branches the
	// expires_at math so the sentinel persists rather than being
	// overwritten by now + 0. The reviewer should still see a
	// prominent lifetime cue on the approval prompt (the approval
	// surfaces print "Lifetime: always" on standing-task expansions)
	// because broadening a permanent grant is higher blast radius
	// than broadening a session.

	parentEnv, err := runtimetasks.EnvelopeFromTask(task)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not load task envelope")
		return
	}
	merge := runtimetasks.MergeEnvelopes(parentEnv, additions)

	// Validate derived gateway scopes BEFORE landing the pending state.
	// Each ExpectedTool whose tool_name parses as "service:action" will
	// be materialized as an AuthorizedAction on approve (see
	// mergeAuthorizedActionsFromExpansion), so it must pass the same
	// service-exists / adapter-supports / service-activated / OAuth-scope
	// / hardcoded-approval gates that Create runs up front. Without this
	// check, a user could approve a (service, action) pair that Create
	// would have rejected — and the expanded scope would be unusable
	// after the round-trip.
	//
	// EXCEPT when the parent task already has a same-service wildcard
	// covering the addition: the merge silently drops the derivation
	// (no new AuthorizedAction is materialized), so the validation
	// gate would otherwise reject a harmless `why`-only refinement of
	// an already-authorized scope. Mirror the wildcard check from
	// mergeAuthorizedActionsFromExpansion so the validation and merge
	// agree on what counts as "redundant."
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
		if detail, status, ok := h.validateDerivedAuthorizedAction(ctx, agent.UserID, service, action, field); !ok {
			writeDetailedError(w, status, detail)
			return
		}
	}

	// Validate added AND replaced credentials. Replacement is keyed
	// per-kind (id vs. handle) so a replacement always preserves the
	// identifier kind — but the literal id/handle string can still
	// change between parent and addition (e.g. agent re-spelled the
	// vault item with an account alias). Without revalidating the New
	// side, an expansion could land a replacement whose new identifier
	// doesn't resolve to a real vault item.
	credsToValidate := append([]runtimetasks.RequiredCredential(nil), merge.AddedCredentials...)
	for _, r := range merge.ReplacedCredentials {
		credsToValidate = append(credsToValidate, r.New)
	}
	if len(credsToValidate) > 0 {
		if err := h.validateTaskRequiredCredentials(ctx, task, credsToValidate); err != nil {
			writeError(w, http.StatusBadRequest, "INVALID_REQUEST", err.Error())
			return
		}
	}

	pending, err := runtimetasks.PendingFromAdditions(additions, req.Reason)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not encode expansion envelope")
		return
	}

	won, err := h.st.SetTaskPendingExpansion(ctx, taskID, pending)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not request scope expansion")
		return
	}
	if !won {
		// Race lost: the row left 'active'/'expired' between our load
		// and the CAS write (cleanup sweep, revocation, concurrent
		// expansion). 409 surfaces this honestly instead of stamping
		// pending_scope_expansion onto a terminal task.
		writeError(w, http.StatusConflict, "INVALID_STATE", "task is no longer in a state that can be expanded; re-fetch and retry")
		return
	}
	task.PendingExpansion = pending
	task.Status = "pending_scope_expansion"
	if err := h.createCanonicalTaskApproval(ctx, task, "task_expand"); err != nil {
		h.logger.ErrorContext(ctx, "failed to create canonical scope expansion approval", "task_id", taskID, "err", err)
	}

	// Send notification.
	if h.notifier != nil {
		approveURL := fmt.Sprintf("%s/dashboard/tasks?action=expand_approve&task_id=%s", h.baseURL, taskID)
		denyURL := fmt.Sprintf("%s/dashboard/tasks?action=expand_deny&task_id=%s", h.baseURL, taskID)

		derivedByKey := indexDerivedActionsByKey(task.AuthorizedActions, task.IntentVerificationMode, additions.ExpectedTools)
		// Pre-stamp the notification with the deterministic
		// envelope-risk score for the MERGED envelope. The approver
		// sees the level the expanded scope would land at — not the
		// stale parent-creation level. Empty when the assessor
		// rejects the request shape (handled lazily by the renderers).
		notifyRiskLevel := ""
		if assessment := runtimepolicy.AssessTaskEnvelope(task.Purpose, merge.Merged); assessment != nil {
			notifyRiskLevel = assessment.RiskLevel
		}
		if msgID, err := h.notifier.SendScopeExpansionRequest(ctx, notify.ScopeExpansionRequest{
			TaskID:              taskID,
			UserID:              agent.UserID,
			AgentName:           agent.Name,
			Purpose:             task.Purpose,
			AddedTools:          notifyExpansionTools(merge.AddedTools, derivedByKey),
			ReplacedTools:       notifyReplacedExpansionTools(merge.ReplacedTools, derivedByKey),
			AddedEgress:         notifyExpansionEgress(merge.AddedEgress),
			ReplacedEgress:      notifyReplacedExpansionEgress(merge.ReplacedEgress),
			AddedCredentials:    notifyExpansionCredentials(merge.AddedCredentials),
			ReplacedCredentials: notifyReplacedExpansionCredentials(merge.ReplacedCredentials),
			Reason:              req.Reason,
			RiskLevel:           notifyRiskLevel,
			Lifetime:            task.Lifetime,
			ApproveURL:          approveURL,
			DenyURL:             denyURL,
		}); err != nil {
			h.logger.WarnContext(ctx, "failed to send scope expansion notification", "task_id", taskID, "err", err)
		} else if msgID != "" {
			_ = h.st.SaveNotificationMessage(ctx, "task", taskID, "telegram", msgID)
		}
	}

	h.publishTasksAndQueue(agent.UserID)

	// If wait=true, long-poll until the expansion is approved or denied.
	if r.URL.Query().Get("wait") == "true" && h.eventHub != nil {
		resolved := h.waitForTaskResolution(ctx, taskID, agent.UserID, longPollDeadline(r))
		if r.Context().Err() != nil {
			return
		}
		sanitizeTaskForResponse(resolved)
		writeJSON(w, http.StatusOK, resolved)
		return
	}

	writeJSON(w, http.StatusAccepted, map[string]any{
		"task_id": taskID,
		"status":  "pending_scope_expansion",
		"message": "Scope expansion requested. Waiting for approval.",
	})
}

// notifyExpansion* helpers translate envelope merge results from the
// runtime tasks types into the pkg/notify wire shape. AGENTS.md keeps
// pkg/ free of internal/ imports, so the translation has to live in
// the handler (which already imports both sides). Each helper is a
// straight field-by-field copy — kept narrow so the boundary stays
// auditable.
//
// For tool entries, we also fold in the auto-execute disposition each
// derived gateway scope would land with. Without this, Telegram-only
// approvers see only the tool_name + why and have no signal whether
// approve grants unmediated execution. The lookup map is keyed on
// "service:action" (lowercased).
func notifyExpansionTools(in []runtimetasks.ExpectedTool, lookup derivedActionLookup) []notify.ExpansionTool {
	if len(in) == 0 {
		return nil
	}
	out := make([]notify.ExpansionTool, len(in))
	for i, t := range in {
		out[i] = expansionToolEntry(t, lookup)
	}
	return out
}

func notifyReplacedExpansionTools(in []runtimetasks.ReplacedExpectedTool, lookup derivedActionLookup) []notify.ReplacedExpansionTool {
	if len(in) == 0 {
		return nil
	}
	out := make([]notify.ReplacedExpansionTool, len(in))
	for i, r := range in {
		out[i] = notify.ReplacedExpansionTool{
			Prior: notify.ExpansionTool{ToolName: r.Prior.ToolName, Why: r.Prior.Why},
			New:   expansionToolEntry(r.New, lookup),
		}
	}
	return out
}

// expansionToolEntry stamps the gateway-action / auto-execute disposition
// onto an outgoing ExpansionTool. Local-harness tools (no service:action
// shape) leave the gateway/auto fields zero.
//
// When the addition is covered by a parent same-service wildcard the
// merger DROPS the specific derivation (the wildcard's broader scope
// already authorizes it); we'd otherwise be left with no derived
// entry and surfaces would render a misleading "needs per-call
// approval" pill. Set WildcardCovered=true and inherit the wildcard's
// AutoExecute so renderers can show the actual effective disposition.
func expansionToolEntry(t runtimetasks.ExpectedTool, lookup derivedActionLookup) notify.ExpansionTool {
	entry := notify.ExpansionTool{ToolName: t.ToolName, Why: t.Why}
	service, action, ok := parseToolNameAsServiceAction(t.ToolName)
	if !ok {
		return entry
	}
	entry.GatewayAction = true
	if a, found := lookup.derived[authorizedActionKey(service, action)]; found {
		entry.AutoExecute = a.AutoExecute
		return entry
	}
	if a, covered := lookup.wildcardByService[strings.ToLower(strings.TrimSpace(service))]; covered {
		entry.AutoExecute = a.AutoExecute
		entry.WildcardCovered = true
	}
	return entry
}

// derivedActionLookup carries the two indices renderers need to label
// each addition correctly: the per-(service,action) derived entries
// from the merge, plus the parent's wildcard entries keyed by
// lowercase service. The wildcard map is what lets the
// "wildcard-covered" branch in expansionToolEntry surface the
// correct disposition for additions whose specific derivation was
// dropped by mergeAuthorizedActionsFromExpansion.
type derivedActionLookup struct {
	derived           map[string]store.TaskAction
	wildcardByService map[string]store.TaskAction
}

// indexDerivedActionsByKey precomputes the (service, action) →
// AuthorizedAction lookup for the additions' derived gateway scopes
// AND the parent's same-service wildcard map. Reuses
// mergeAuthorizedActionsFromExpansion so the notification surface
// sees the same disposition the approve path will persist. Keyed via
// authorizedActionKey so the lookup matches regardless of the
// caller's casing.
func indexDerivedActionsByKey(parent []store.TaskAction, taskIntentVerification string, additions []runtimetasks.ExpectedTool) derivedActionLookup {
	merged := mergeAuthorizedActionsFromExpansion(parent, taskIntentVerification, additions)
	derived := make(map[string]store.TaskAction, len(merged))
	for _, a := range merged {
		derived[authorizedActionKey(a.Service, a.Action)] = a
	}
	wildcards := make(map[string]store.TaskAction)
	for _, a := range parent {
		if a.Action == "*" {
			wildcards[strings.ToLower(strings.TrimSpace(a.Service))] = a
		}
	}
	return derivedActionLookup{derived: derived, wildcardByService: wildcards}
}

func notifyExpansionEgress(in []runtimetasks.ExpectedEgress) []notify.ExpansionEgress {
	if len(in) == 0 {
		return nil
	}
	out := make([]notify.ExpansionEgress, len(in))
	for i, e := range in {
		out[i] = notify.ExpansionEgress{Host: e.Host, Why: e.Why}
	}
	return out
}

func notifyReplacedExpansionEgress(in []runtimetasks.ReplacedExpectedEgress) []notify.ReplacedExpansionEgress {
	if len(in) == 0 {
		return nil
	}
	out := make([]notify.ReplacedExpansionEgress, len(in))
	for i, r := range in {
		out[i] = notify.ReplacedExpansionEgress{
			Prior: notify.ExpansionEgress{Host: r.Prior.Host, Why: r.Prior.Why},
			New:   notify.ExpansionEgress{Host: r.New.Host, Why: r.New.Why},
		}
	}
	return out
}

func notifyExpansionCredentials(in []runtimetasks.RequiredCredential) []notify.ExpansionCredential {
	if len(in) == 0 {
		return nil
	}
	out := make([]notify.ExpansionCredential, len(in))
	for i, c := range in {
		out[i] = notify.ExpansionCredential{
			VaultItemID:     c.VaultItemID,
			VaultItemHandle: c.VaultItemHandle,
			Why:             c.Why,
		}
	}
	return out
}

func notifyReplacedExpansionCredentials(in []runtimetasks.ReplacedRequiredCredential) []notify.ReplacedExpansionCredential {
	if len(in) == 0 {
		return nil
	}
	out := make([]notify.ReplacedExpansionCredential, len(in))
	for i, r := range in {
		out[i] = notify.ReplacedExpansionCredential{
			Prior: notify.ExpansionCredential{
				VaultItemID:     r.Prior.VaultItemID,
				VaultItemHandle: r.Prior.VaultItemHandle,
				Why:             r.Prior.Why,
			},
			New: notify.ExpansionCredential{
				VaultItemID:     r.New.VaultItemID,
				VaultItemHandle: r.New.VaultItemHandle,
				Why:             r.New.Why,
			},
		}
	}
	return out
}

// envelopeHasAdditions reports whether at least one envelope field
// carries a non-empty entry. Used by Expand to reject empty bodies
// before reaching the validator, which would otherwise let an empty
// body silently flip the task to pending_scope_expansion with no
// actual delta.
func envelopeHasAdditions(env runtimetasks.Envelope) bool {
	return len(env.ExpectedTools) > 0 || len(env.ExpectedEgress) > 0 || len(env.RequiredCredentials) > 0
}

// ExpandApprove approves a pending scope expansion.
//
// POST /api/tasks/{id}/expand/approve
// Auth: user JWT
func (h *TasksHandler) ExpandApprove(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user := middleware.UserFromContext(ctx)
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}

	taskID := r.PathValue("id")
	task, err := h.st.GetTask(ctx, taskID)
	if err != nil {
		if err == store.ErrNotFound {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "task not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not get task")
		return
	}
	if task.UserID != user.ID {
		writeError(w, http.StatusForbidden, "FORBIDDEN", "not your task")
		return
	}
	if task.Status != "pending_scope_expansion" || task.PendingExpansion == nil {
		writeError(w, http.StatusConflict, "INVALID_STATE", "task has no pending scope expansion")
		return
	}

	envUpdate, merged, err := buildExpansionApprovalUpdate(task)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
		return
	}
	reassessExpansionRisk(task, merged, &envUpdate)
	// Snapshot the pending row we read so the CAS rejects writes
	// computed from a stale pending — protects against a deny that
	// clears pending_expansion_json AND a subsequent expand that
	// replaces it before our approve write lands. Without this the
	// CAS would still succeed (status would match again) and a
	// stale merged envelope would silently overwrite the new
	// pending state. Marshal failure is fatal: silently skipping the
	// guard would disable stale-approve protection on the same
	// approval that hit the marshal bug.
	pendingJSON, mErr := json.Marshal(task.PendingExpansion)
	if mErr != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not snapshot pending expansion for CAS guard")
		return
	}
	envUpdate.ExpectedPendingJSON = pendingJSON
	// Standing tasks keep the far-future sentinel that Approve uses
	// (taskCredentialExpiry mirrors the same shape). Without this
	// branch, ExpiresInSeconds=0 would land expires_at=now on the
	// row and the task would immediately collapse to expired.
	expiresAt := taskApprovedExpiresAt(task)

	// CAS guards on (status='pending_scope_expansion', pending
	// snapshot matches) so a concurrent ExpandDeny+re-expand
	// sequence cannot let a stale approve land its merged envelope.
	won, err := h.st.UpdateTaskEnvelopeFrom(ctx, taskID, "pending_scope_expansion", envUpdate, expiresAt)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not expand task")
		return
	}
	if !won {
		writeError(w, http.StatusConflict, "ALREADY_RESOLVED", "scope expansion was resolved by another caller")
		return
	}
	h.resolveCanonicalTaskApproval(ctx, task, "task_expand", taskApprovalResolution(task), "approved")

	// Deliver callback if set.
	if task.CallbackURL != nil && *task.CallbackURL != "" {
		cbKey, _ := h.st.GetAgentCallbackSecret(ctx, task.AgentID)
		h.dispatchCallback(*task.CallbackURL, &callback.Payload{
			Type:   "task",
			TaskID: taskID,
			Status: "scope_expanded",
		}, cbKey)
	}

	h.publishTasksAndQueue(user.ID)

	resp := map[string]any{
		"task_id": taskID,
		"status":  "active",
	}
	if task.Lifetime != "standing" {
		resp["expires_at"] = expiresAt.Format(time.RFC3339)
	}
	writeJSON(w, http.StatusOK, resp)
}

// taskApprovedExpiresAt returns the expires_at the approve path
// should land on the task row. Standing tasks keep the far-future
// sentinel; session/sliding tasks get a fresh
// ExpiresInSeconds-from-now deadline. Shared by every Approve-class
// surface — direct Approve, ApproveByTaskID, ApproveInlineTask,
// ExpandApprove, ExpandApproveByTaskID, ApproveInlineExpansion —
// so the standing-task math has exactly one definition.
func taskApprovedExpiresAt(task *store.Task) time.Time {
	if task != nil && task.Lifetime == "standing" {
		return time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
	}
	return time.Now().UTC().Add(time.Duration(task.ExpiresInSeconds) * time.Second)
}

// buildExpansionApprovalUpdate translates a task's PendingExpansion into
// the TaskEnvelopeUpdate that UpdateTaskEnvelope wants. It applies
// replace-by-name dedup against the parent envelope (so the persisted
// envelope has at most one entry per canonical key) AND derives new
// AuthorizedActions from any ExpectedTool whose tool_name parses as
// "service:action". The derived actions go through the same (service,
// action) replace-by-name dedup against the parent's AuthorizedActions
// — without this, the gateway's CheckTaskScope (which reads only
// AuthorizedActions) would refuse the newly approved scope even though
// the envelope shows it.
//
// Also returns the merged envelope so callers can run a fresh risk
// assessment on it (the create-time risk is stale once new
// scope lands; an expansion can transform a low-risk read-only task
// into a high-risk mutating one).
func buildExpansionApprovalUpdate(task *store.Task) (store.TaskEnvelopeUpdate, runtimetasks.Envelope, error) {
	parentEnv, err := runtimetasks.EnvelopeFromTask(task)
	if err != nil {
		return store.TaskEnvelopeUpdate{}, runtimetasks.Envelope{}, fmt.Errorf("load parent envelope: %w", err)
	}
	additions, err := pendingExpansionToEnvelope(task.PendingExpansion)
	if err != nil {
		return store.TaskEnvelopeUpdate{}, runtimetasks.Envelope{}, err
	}
	merge := runtimetasks.MergeEnvelopes(parentEnv, additions)
	toolsRaw, egressRaw, credsRaw, err := runtimetasks.EnvelopeToRawColumns(merge.Merged)
	if err != nil {
		return store.TaskEnvelopeUpdate{}, runtimetasks.Envelope{}, fmt.Errorf("encode merged envelope: %w", err)
	}
	return store.TaskEnvelopeUpdate{
		AuthorizedActions:   mergeAuthorizedActionsFromExpansion(task.AuthorizedActions, task.IntentVerificationMode, additions.ExpectedTools),
		ExpectedTools:       toolsRaw,
		ExpectedEgress:      egressRaw,
		RequiredCredentials: credsRaw,
	}, merge.Merged, nil
}

// reassessExpansionRisk runs the deterministic envelope-shape risk
// policy on the merged envelope and stamps the result into envUpdate.
// Cheap, synchronous, no LLM call — every approve gets the structural
// amplifiers (wildcard hosts, regex matchers, intent verification off)
// re-scored, so a low-risk task can't quietly accumulate destructive
// scope across expansions without the risk badge moving.
//
// The deeper LLM assessor is intentionally NOT invoked here: it would
// add multi-second latency to a button click. The follow-up
// risk-baseline PR can layer that on once it has a story for the
// latency budget.
func reassessExpansionRisk(task *store.Task, merged runtimetasks.Envelope, envUpdate *store.TaskEnvelopeUpdate) {
	if task == nil || envUpdate == nil {
		return
	}
	assessment := runtimepolicy.AssessTaskEnvelope(task.Purpose, merged)
	if assessment == nil || assessment.RiskLevel == "" {
		return
	}
	envUpdate.RiskLevel = assessment.RiskLevel
	envUpdate.RiskDetails = taskrisk.MarshalAssessment(assessment)
}

// validateDerivedAuthorizedAction mirrors Create's per-action gates
// (adapter-known service, supported action, service activated, OAuth
// scopes) for an AuthorizedAction that the Expand approve path will
// materialize from an ExpectedTool with a `service:action` name. The
// expand path uses this BEFORE landing the pending state so an
// unusable scope is rejected up front rather than after the user wastes
// approval time on a verdict the dashboard would have refused at create.
//
// The hardcoded-approval gate Create runs is intentionally skipped:
// derived NEW actions always land with AutoExecute=false (see
// mergeAuthorizedActionsFromExpansion), so the hardcoded-approval
// safety property — "this action ALWAYS requires per-call approval" —
// is already satisfied by the default posture. If the AutoExecute
// default ever flips, restore the RequiresHardcodedApproval check here.
//
// field is the JSON path of the offending entry (e.g. "expected_tools[2]")
// — surfaced in the error so the agent knows which entry to fix.
//
// Returns (apiErrorDetail, status, false) on a failed gate; the caller
// writes the response. Returns ok=true with a zero detail on pass.
func (h *TasksHandler) validateDerivedAuthorizedAction(ctx context.Context, userID, service, action, field string) (apiErrorDetail, int, bool) {
	if service == "" || action == "" {
		return apiErrorDetail{
			Error: fmt.Sprintf("%s.tool_name parses as service:action but service or action is empty", field),
			Code:  "INVALID_REQUEST",
			Hint:  "Use the form \"<service>:<action>\" with both parts non-empty (e.g. \"github:create_issue\").",
		}, http.StatusBadRequest, false
	}
	if action == "*" {
		// Wildcard authorizations are intentionally NOT allowed via
		// expansion. The dashboard would surface them with the same
		// auto-execute pill as a specific action, while hardcoded
		// actions under the wildcard would still gate at call time —
		// a misleading approval prompt. Agents must enumerate the
		// specific actions they need.
		return apiErrorDetail{
			Error: fmt.Sprintf("%s.tool_name uses wildcard action; expansions must name a specific action", field),
			Code:  "INVALID_REQUEST",
			Hint:  "Replace \"*\" with the specific action you need (e.g. \"github:create_issue\"). To grant wildcard scope, create a new task with explicit authorized_actions.",
		}, http.StatusBadRequest, false
	}
	serviceType, serviceAlias := parseServiceAlias(service)

	if isLocalService(serviceType) {
		if err := h.validateLocalService(ctx, userID, serviceType, action); err != nil {
			return apiErrorDetail{
				Error: fmt.Sprintf("%s: %s", field, err.Error()),
				Code:  "INVALID_REQUEST",
			}, http.StatusBadRequest, false
		}
		return apiErrorDetail{}, http.StatusOK, true
	}

	if isGuardVirtualService(serviceType) {
		// Guard virtual services bypass adapter lookup at create — same
		// here. Return ok to mirror Create's behavior.
		return apiErrorDetail{}, http.StatusOK, true
	}

	adapter, ok := h.adapterReg.GetForUser(ctx, serviceType, userID)
	if !ok {
		available := h.adapterReg.SupportedServices()
		var ids []string
		for _, s := range available {
			ids = append(ids, s.ID)
		}
		return apiErrorDetail{
			Error:     fmt.Sprintf("%s: unknown service %q", field, service),
			Code:      "INVALID_REQUEST",
			Hint:      "Use the service ID from the catalog (GET /api/skill/catalog).",
			Available: ids,
		}, http.StatusBadRequest, false
	}
	if action != "*" && !adapterSupportsAction(adapter, action) {
		return apiErrorDetail{
			Error:     fmt.Sprintf("%s: service %q does not support action %q", field, serviceType, action),
			Code:      "INVALID_REQUEST",
			Hint:      "Use \"*\" to authorize all actions, or pick from the supported actions for this service.",
			Available: adapter.SupportedActions(),
		}, http.StatusBadRequest, false
	}
	if !h.serviceActivated(ctx, userID, serviceType, serviceAlias, adapter) {
		code, userErr, _ := serviceNotActivatedResponse(ctx, h.vault, h.st, h.adapterReg, userID, serviceType, serviceAlias, service, adapter)
		return apiErrorDetail{
			Error: fmt.Sprintf("%s: %s", field, userErr),
			Code:  code,
		}, http.StatusBadRequest, false
	}
	if missing := h.missingCredentialScopes(ctx, userID, serviceType, serviceAlias, action, adapter); len(missing) > 0 {
		return apiErrorDetail{
			Error: fmt.Sprintf("%s: service %q is connected but missing required OAuth scopes: %s — the user needs to reconnect the service to grant these permissions",
				field, service, strings.Join(missing, ", ")),
			Code: "MISSING_SCOPES",
		}, http.StatusBadRequest, false
	}
	return apiErrorDetail{}, http.StatusOK, true
}

// mergeAuthorizedActionsFromExpansion derives new AuthorizedActions
// from the additions' ExpectedTools and merges them with the parent's
// AuthorizedActions using (service, action) dedup.
//
// Derivation rules:
//   - tool_name parses as "service:action" → AuthorizedAction{Service,
//     Action, ExpansionRationale=why}. The colon split picks the LAST
//     colon so service ids containing aliases (e.g. "github:personal:list_repos"
//     → service="github:personal", action="list_repos") work as expected.
//   - tool_name without a colon (e.g. "Bash", "Edit") is a local tool —
//     no AuthorizedAction derived. The envelope still carries the entry,
//     but the gateway path doesn't gate on it.
//   - If the parent already has a same-service wildcard
//     ({service, "*"}) the derived action is REDUNDANT (the wildcard
//     already covers it) — we skip the derivation entirely rather
//     than appending a specific row that would render an
//     AutoExecute=false pill alongside the wildcard's broader grant.
//     The wildcard's existing ExpansionRationale is left untouched
//     because a specific tool's `why` doesn't accurately describe
//     the wildcard's whole-service grant.
//
// Replace-by-name (same service+action) overwrites the existing entry's
// ExpansionRationale with the new per-entry why so intent verification
// later sees the most recent rationale for the granted scope.
// AutoExecute on a REPLACED action is preserved from the matching
// parent entry to honor any prior dashboard tuning.
//
// AutoExecute on a derived NEW action defaults to FALSE — every
// per-call gate stays on until the user explicitly relaxes it via the
// dashboard's scope overrides. We deliberately do NOT inherit from
// the parent's same-service entries: a benign `github:list_issues`
// auto-execute approval should not silently extend to a destructive
// `github:delete_repo`. The hardcoded-approval allowlist is unlikely
// to be complete across every (service, action) pair, so it cannot
// be the only safety net. The dashboard approval prompt surfaces the
// "per-call approval" disposition pill so the reviewer can decide
// whether to relax it after approving.
//
// Verification on a derived NEW action inherits the task's
// IntentVerificationMode (or "strict" if empty). A blank Verification
// would silently defer to whatever the runtime treats as default,
// drifting in audit trails versus the explicit values on the
// task's create-time actions.
func mergeAuthorizedActionsFromExpansion(parent []store.TaskAction, taskIntentVerification string, additions []runtimetasks.ExpectedTool) []store.TaskAction {
	// Always return a fresh slice — callers store the result in
	// TaskEnvelopeUpdate / map values, and any future caller-side
	// mutation must not aliasingly modify the original task's
	// AuthorizedActions. The cost of a clone-on-empty is trivial; the
	// cost of a future aliasing bug is not.
	out := slices.Clone(parent)
	if len(additions) == 0 {
		return out
	}
	index := make(map[string]int, len(out))
	wildcardServices := make(map[string]struct{})
	for i, a := range out {
		index[authorizedActionKey(a.Service, a.Action)] = i
		if a.Action == "*" {
			wildcardServices[strings.ToLower(strings.TrimSpace(a.Service))] = struct{}{}
		}
	}
	derivedVerification := strings.TrimSpace(taskIntentVerification)
	if derivedVerification == "" {
		derivedVerification = "strict"
	}
	for _, tool := range additions {
		service, action, ok := parseToolNameAsServiceAction(tool.ToolName)
		if !ok {
			continue
		}
		key := authorizedActionKey(service, action)
		why := strings.TrimSpace(tool.Why)
		if idx, exists := index[key]; exists {
			out[idx].ExpansionRationale = why
			continue
		}
		if _, covered := wildcardServices[strings.ToLower(strings.TrimSpace(service))]; covered {
			// Same-service wildcard already authorizes this action;
			// appending a specific entry would render a confusing
			// AutoExecute=false pill next to a wildcard the user
			// already granted. Skip — the wildcard's existing
			// ExpansionRationale stays accurate for its whole-service
			// scope.
			continue
		}
		index[key] = len(out)
		out = append(out, store.TaskAction{
			Service:            service,
			Action:             action,
			AutoExecute:        false,
			Verification:       derivedVerification,
			ExpansionRationale: why,
		})
	}
	return out
}

// authorizedActionKey returns the canonical dedup key for (service,
// action). Lowercased so the AuthorizedActions dedup and the
// ExpectedTools dedup (which lowercases tool_name) agree — otherwise
// an addition with mismatched casing lands as both a "replacement" in
// the envelope and a "new" AuthorizedAction, silently drifting the
// two surfaces.
func authorizedActionKey(service, action string) string {
	return strings.ToLower(strings.TrimSpace(service)) + ":" + strings.ToLower(strings.TrimSpace(action))
}

// parseToolNameAsServiceAction recognizes tool names shaped as
// "<service>:<action>" and returns the components. Service identifiers
// may themselves contain a colon (e.g. account-aliased
// "google.gmail:work"), so we split on the LAST colon. A bare tool name
// without any colon returns ok=false — those are local-harness tools
// (Bash, Edit, Read, …) and do not authorize a gateway-routed scope.
//
// Empty service or empty action also returns ok=false. Defensive:
// callers iterate user-supplied data and shouldn't materialize a
// half-shaped AuthorizedAction with a blank field that would later
// match anything under one of the lookup paths.
func parseToolNameAsServiceAction(toolName string) (service, action string, ok bool) {
	name := strings.TrimSpace(toolName)
	if name == "" {
		return "", "", false
	}
	idx := strings.LastIndex(name, ":")
	if idx <= 0 || idx == len(name)-1 {
		return "", "", false
	}
	service = strings.TrimSpace(name[:idx])
	action = strings.TrimSpace(name[idx+1:])
	if service == "" || action == "" {
		return "", "", false
	}
	return service, action, true
}

// pendingExpansionToEnvelope decodes a PendingTaskExpansion (raw JSON
// per field) into an Envelope for merging. Decode errors fail-closed
// rather than silently dropping the field: validation at Expand time
// guarantees a well-formed row, so encountering corruption at approve
// time is a system-level problem. Silently approving an empty merge
// while the dashboard shows the original (pre-store) pending JSON would
// commit a different shape than the user thought they approved —
// strictly worse than surfacing a 500 and prompting investigation.
func pendingExpansionToEnvelope(pending *store.PendingTaskExpansion) (runtimetasks.Envelope, error) {
	if pending == nil {
		return runtimetasks.Envelope{}, nil
	}
	var env runtimetasks.Envelope
	if len(pending.ExpectedTools) > 0 {
		if err := json.Unmarshal(pending.ExpectedTools, &env.ExpectedTools); err != nil {
			return runtimetasks.Envelope{}, fmt.Errorf("pending_expansion_json.expected_tools corrupt: %w", err)
		}
	}
	if len(pending.ExpectedEgress) > 0 {
		if err := json.Unmarshal(pending.ExpectedEgress, &env.ExpectedEgress); err != nil {
			return runtimetasks.Envelope{}, fmt.Errorf("pending_expansion_json.expected_egress corrupt: %w", err)
		}
	}
	if len(pending.RequiredCredentials) > 0 {
		if err := json.Unmarshal(pending.RequiredCredentials, &env.RequiredCredentials); err != nil {
			return runtimetasks.Envelope{}, fmt.Errorf("pending_expansion_json.required_credentials corrupt: %w", err)
		}
	}
	return env, nil
}

// ExpandDeny denies a pending scope expansion.
//
// POST /api/tasks/{id}/expand/deny
// Auth: user JWT
func (h *TasksHandler) ExpandDeny(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user := middleware.UserFromContext(ctx)
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}

	taskID := r.PathValue("id")
	task, err := h.st.GetTask(ctx, taskID)
	if err != nil {
		if err == store.ErrNotFound {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "task not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not get task")
		return
	}
	if task.UserID != user.ID {
		writeError(w, http.StatusForbidden, "FORBIDDEN", "not your task")
		return
	}
	if task.Status != "pending_scope_expansion" {
		writeError(w, http.StatusConflict, "INVALID_STATE", "task has no pending scope expansion")
		return
	}

	// Revert to active (or expired if it was expired before).
	newStatus := store.ResolveExpansionStatusActive
	if task.ExpiresAt != nil && time.Now().After(*task.ExpiresAt) {
		newStatus = store.ResolveExpansionStatusExpired
	}

	won, err := h.st.ResolveTaskPendingExpansion(ctx, taskID, newStatus)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not deny expansion")
		return
	}
	if !won {
		writeError(w, http.StatusConflict, "ALREADY_RESOLVED", "scope expansion was resolved by another caller")
		return
	}
	h.resolveCanonicalTaskApproval(ctx, task, "task_expand", "deny", "denied")

	h.publishTasksAndQueue(user.ID)

	// Deliver callback if set.
	if task.CallbackURL != nil && *task.CallbackURL != "" {
		cbKey, _ := h.st.GetAgentCallbackSecret(ctx, task.AgentID)
		h.dispatchCallback(*task.CallbackURL, &callback.Payload{
			Type:   "task",
			TaskID: taskID,
			Status: "scope_expansion_denied",
		}, cbKey)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"task_id": taskID,
		"status":  newStatus,
	})
}

// ── Core approve/deny methods (used by HTTP handlers and Telegram consumer) ──

// ApproveByTaskID approves a pending task.
func (h *TasksHandler) ApproveByTaskID(ctx context.Context, taskID, userID string) error {
	task, err := h.st.GetTask(ctx, taskID)
	if err != nil {
		return err
	}
	if task.UserID != userID {
		return fmt.Errorf("not your task")
	}
	if isInlineChatPending(task) {
		// Chat-bound pending task; the cache hold owns the in-process
		// resolution path. Notifier callers (Telegram button) see this
		// error and surface it; the chat surface uses ApproveInlineTask
		// instead which bypasses this guard.
		return errInlineChatBound
	}
	if task.Status != "pending_approval" {
		if task.Status == "active" {
			requiredCredentials, parseErr := taskRequiredCredentials(task)
			if parseErr != nil {
				return fmt.Errorf("could not parse required_credentials")
			}
			if len(requiredCredentials) > 0 {
				if err := h.validateTaskRequiredCredentials(ctx, task, requiredCredentials); err != nil {
					return err
				}
				if _, ensureErr := h.ensureTaskCredentialPlaceholders(ctx, task, requiredCredentials, taskCredentialExpiry(task)); ensureErr != nil {
					return ensureErr
				}
				h.resolveCanonicalTaskApproval(ctx, task, "task_create", taskApprovalResolution(task), "approved")
				h.updateNotificationMsg(ctx, "task", taskID, userID, "✅ <b>Approved</b> — task activated.")
				h.publishTasksAndQueue(userID)
				return nil
			}
		}
		return fmt.Errorf("task is not pending approval")
	}
	requiredCredentials, err := taskRequiredCredentials(task)
	if err != nil {
		return fmt.Errorf("could not parse required_credentials")
	}
	if err := h.validateTaskRequiredCredentials(ctx, task, requiredCredentials); err != nil {
		return err
	}

	expiresAt := taskApprovedExpiresAt(task)
	if _, err := h.ensureTaskCredentialPlaceholders(ctx, task, requiredCredentials, expiresAt); err != nil {
		return err
	}
	won, err := h.st.UpdateTaskApprovedFrom(ctx, taskID, "pending_approval", expiresAt, task.AuthorizedActions)
	if err != nil {
		return err
	}
	if !won {
		return fmt.Errorf("task is no longer pending approval")
	}
	h.resolveCanonicalTaskApproval(ctx, task, "task_create", taskApprovalResolution(task), "approved")

	h.updateNotificationMsg(ctx, "task", taskID, userID, "✅ <b>Approved</b> — task activated.")
	h.publishTasksAndQueue(userID)

	if task.CallbackURL != nil && *task.CallbackURL != "" {
		cbKey, _ := h.st.GetAgentCallbackSecret(ctx, task.AgentID)
		h.dispatchCallback(*task.CallbackURL, &callback.Payload{
			Type:   "task",
			TaskID: taskID,
			Status: "approved",
		}, cbKey)
	}
	return nil
}

// DenyByTaskID denies a pending task.
func (h *TasksHandler) DenyByTaskID(ctx context.Context, taskID, userID string) error {
	task, err := h.st.GetTask(ctx, taskID)
	if err != nil {
		return err
	}
	if task.UserID != userID {
		return fmt.Errorf("not your task")
	}
	// Chat-bound pending rows are intentionally denyable from this
	// path so the dashboard / notifier can dismiss zombie tasks the
	// agent has lost track of. The chat-side approve path handles
	// the "already terminal" case explicitly. Approve, by contrast,
	// remains chat-only (see ApproveByTaskID's guard).
	if task.Status != "pending_approval" && task.Status != "pending_scope_expansion" {
		return fmt.Errorf("task is not pending")
	}

	// Route pending_scope_expansion denies through
	// ResolveTaskPendingExpansion so pending_expansion_json clears
	// atomically — see the Deny handler comment for the invariant.
	won, err := h.denyTaskInState(ctx, taskID, task.Status)
	if err != nil {
		return err
	}
	if !won {
		return fmt.Errorf("task is no longer pending")
	}
	h.resolveCanonicalTaskApproval(ctx, task, canonicalTaskApprovalKind(task), "deny", "denied")

	h.updateNotificationMsg(ctx, "task", taskID, userID, "❌ <b>Denied</b> — task rejected.")
	h.decrementNotifierPolling(userID)
	h.publishTasksAndQueue(userID)

	if task.CallbackURL != nil && *task.CallbackURL != "" {
		cbKey, _ := h.st.GetAgentCallbackSecret(ctx, task.AgentID)
		h.dispatchCallback(*task.CallbackURL, &callback.Payload{
			Type:   "task",
			TaskID: taskID,
			Status: "denied",
		}, cbKey)
	}
	return nil
}

// ExpandApproveByTaskID approves a pending scope expansion.
func (h *TasksHandler) ExpandApproveByTaskID(ctx context.Context, taskID, userID string) error {
	task, err := h.st.GetTask(ctx, taskID)
	if err != nil {
		return err
	}
	if task.UserID != userID {
		return fmt.Errorf("not your task")
	}
	if task.Status != "pending_scope_expansion" || task.PendingExpansion == nil {
		return fmt.Errorf("task has no pending scope expansion")
	}

	envUpdate, merged, err := buildExpansionApprovalUpdate(task)
	if err != nil {
		return err
	}
	reassessExpansionRisk(task, merged, &envUpdate)
	// See the ExpandApprove handler — same pending-snapshot CAS
	// guard, same fail-closed marshal handling.
	pendingJSON, mErr := json.Marshal(task.PendingExpansion)
	if mErr != nil {
		return fmt.Errorf("snapshot pending expansion: %w", mErr)
	}
	envUpdate.ExpectedPendingJSON = pendingJSON
	expiresAt := taskApprovedExpiresAt(task)
	won, err := h.st.UpdateTaskEnvelopeFrom(ctx, taskID, "pending_scope_expansion", envUpdate, expiresAt)
	if err != nil {
		return err
	}
	if !won {
		// Concurrent resolution lost the race; surface a clean error
		// to the Telegram/notifier callback so the caller can refresh
		// state instead of believing it approved.
		return fmt.Errorf("scope expansion was resolved by another caller")
	}
	h.resolveCanonicalTaskApproval(ctx, task, "task_expand", taskApprovalResolution(task), "approved")

	h.updateNotificationMsg(ctx, "task", taskID, userID, "✅ <b>Scope expanded</b>")
	h.publishTasksAndQueue(userID)

	if task.CallbackURL != nil && *task.CallbackURL != "" {
		cbKey, _ := h.st.GetAgentCallbackSecret(ctx, task.AgentID)
		h.dispatchCallback(*task.CallbackURL, &callback.Payload{
			Type:   "task",
			TaskID: taskID,
			Status: "scope_expanded",
		}, cbKey)
	}
	return nil
}

// ExpandDenyByTaskID denies a pending scope expansion.
func (h *TasksHandler) ExpandDenyByTaskID(ctx context.Context, taskID, userID string) error {
	task, err := h.st.GetTask(ctx, taskID)
	if err != nil {
		return err
	}
	if task.UserID != userID {
		return fmt.Errorf("not your task")
	}
	if task.Status != "pending_scope_expansion" {
		return fmt.Errorf("task has no pending scope expansion")
	}

	newStatus := store.ResolveExpansionStatusActive
	if task.ExpiresAt != nil && time.Now().After(*task.ExpiresAt) {
		newStatus = store.ResolveExpansionStatusExpired
	}
	// Atomic clear+status; CAS on pending_scope_expansion so a
	// concurrent approve that landed first can't be clobbered.
	won, err := h.st.ResolveTaskPendingExpansion(ctx, taskID, newStatus)
	if err != nil {
		return err
	}
	if !won {
		return fmt.Errorf("scope expansion was resolved by another caller")
	}
	h.resolveCanonicalTaskApproval(ctx, task, "task_expand", "deny", "denied")

	h.updateNotificationMsg(ctx, "task", taskID, userID, "❌ <b>Scope expansion denied</b>")
	h.decrementNotifierPolling(userID)
	h.publishTasksAndQueue(userID)

	if task.CallbackURL != nil && *task.CallbackURL != "" {
		cbKey, _ := h.st.GetAgentCallbackSecret(ctx, task.AgentID)
		h.dispatchCallback(*task.CallbackURL, &callback.Payload{
			Type:   "task",
			TaskID: taskID,
			Status: "scope_expansion_denied",
		}, cbKey)
	}
	return nil
}

// decrementNotifierPolling calls DecrementPolling on the notifier if it supports it.
func (h *TasksHandler) decrementNotifierPolling(userID string) {
	if h.notifier == nil {
		return
	}
	if pd, ok := h.notifier.(notify.PollingDecrementer); ok {
		pd.DecrementPolling(userID)
	}
}

// errInlineChatBound is returned by the dashboard APPROVE path when
// the caller targets a chat-bound pending task. Approval requires the
// in-process cache hold to land coherently — granting scope and
// credentials via the dashboard would leave the model unaware of the
// transition. The DENY path intentionally does NOT gate on this:
// users must be able to dismiss zombie chat-bound tasks even when
// the conversation has moved on, and the chat-side approve handler
// detects the resulting already-terminal state via
// llmproxy.ErrInlineTaskAlreadyTerminal. ApproveInlineTask in
// tasks_inline.go bypasses this guard because the chat surface IS
// the legitimate caller.
var errInlineChatBound = errors.New("approve in the agent chat surface")

// isInlineChatPending reports whether a task is awaiting an APPROVE
// decision via the chat surface. Read by the dashboard Approve
// handler (and ApproveByTaskID notifier callback) as a 409 gate.
// Deny does not consult it.
func isInlineChatPending(task *store.Task) bool {
	if task == nil {
		return false
	}
	return task.Status == "pending_approval" && task.ApprovalSource == "inline_chat"
}

func canonicalTaskApprovalKind(task *store.Task) string {
	if task.Status == "pending_scope_expansion" || task.PendingExpansion != nil {
		return "task_expand"
	}
	return "task_create"
}

func taskApprovalResolution(task *store.Task) string {
	if task.Lifetime == "standing" {
		return "allow_always"
	}
	return "allow_session"
}

func (h *TasksHandler) createCanonicalTaskApproval(ctx context.Context, task *store.Task, kind string) error {
	payload, err := json.Marshal(task)
	if err != nil {
		return err
	}

	summary := map[string]any{
		"purpose":    task.Purpose,
		"lifetime":   task.Lifetime,
		"risk_level": task.RiskLevel,
	}
	if kind == "task_expand" && task.PendingExpansion != nil {
		// Surface the envelope diff at the same fidelity as the
		// inline / Telegram approval prompts so a dashboard reviewer
		// can audit the same shape the user-facing approval surfaces
		// show.
		additions, decErr := pendingExpansionToEnvelope(task.PendingExpansion)
		if decErr != nil {
			// Corrupt pending row: surface the issue in the approval
			// record summary so the audit trail shows the decode failed
			// rather than fabricating a clean diff. The approve handler
			// will independently 500 when buildExpansionApprovalUpdate
			// hits the same decode.
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
		Kind:                kind,
		UserID:              task.UserID,
		AgentID:             &task.AgentID,
		TaskID:              &task.ID,
		Status:              "pending",
		Surface:             "dashboard",
		SummaryJSON:         json.RawMessage(summaryJSON),
		PayloadJSON:         json.RawMessage(payload),
		ResolutionTransport: "task_state_update",
	}
	return h.st.CreateApprovalRecord(ctx, rec)
}

func (h *TasksHandler) resolveCanonicalTaskApproval(ctx context.Context, task *store.Task, kind, resolution, status string) {
	rec, err := h.findPendingTaskApprovalRecord(ctx, task.UserID, task.ID, kind)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			h.logger.ErrorContext(ctx, "failed to load canonical task approval", "task_id", task.ID, "kind", kind, "err", err)
		}
		return
	}
	if err := validateApprovalRecordTransition(rec, resolution, status); err != nil {
		h.logger.ErrorContext(ctx, "illegal canonical task approval transition", "task_id", task.ID, "approval_id", rec.ID, "kind", rec.Kind, "from_status", rec.Status, "resolution", resolution, "status", status, "err", err)
		return
	}
	if err := h.st.ResolveApprovalRecord(ctx, rec.ID, resolution, status, time.Now().UTC()); err != nil {
		h.logger.ErrorContext(ctx, "failed to resolve canonical task approval", "task_id", task.ID, "approval_id", rec.ID, "err", err)
	}
}

func (h *TasksHandler) findPendingTaskApprovalRecord(ctx context.Context, userID, taskID, kind string) (*store.ApprovalRecord, error) {
	recs, err := h.st.ListPendingApprovalRecords(ctx, userID)
	if err != nil {
		return nil, err
	}
	var latest *store.ApprovalRecord
	for _, rec := range recs {
		if rec.Kind != kind || rec.TaskID == nil || *rec.TaskID != taskID {
			continue
		}
		if latest == nil || rec.CreatedAt.After(latest.CreatedAt) {
			latest = rec
		}
	}
	if latest == nil {
		return nil, store.ErrNotFound
	}
	return latest, nil
}

// updateNotificationMsg updates the Telegram message for a target
// using the notification_messages table.
func (h *TasksHandler) updateNotificationMsg(ctx context.Context, targetType, targetID, userID, text string) {
	if h.notifier == nil {
		return
	}
	msgID, err := h.st.GetNotificationMessage(ctx, targetType, targetID, "telegram")
	if err != nil {
		return
	}
	if err := h.notifier.UpdateMessage(ctx, userID, msgID, text); err != nil {
		h.logger.WarnContext(ctx, "telegram message update failed", "err", err, "target_type", targetType, "target_id", targetID)
	}
}

// publishTasksAndQueue publishes SSE events for tasks and queue changes.
func (h *TasksHandler) publishTasksAndQueue(userID string) {
	if h.eventHub == nil {
		return
	}
	h.eventHub.Publish(userID, events.Event{Type: "tasks"})
	h.eventHub.Publish(userID, events.Event{Type: "queue"})
}

// ── Revoke ────────────────────────────────────────────────────────────────────

// Revoke cancels an active (typically standing) task.
//
// POST /api/tasks/{id}/revoke
// Auth: user JWT
func (h *TasksHandler) Revoke(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user := middleware.UserFromContext(ctx)
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}

	taskID := r.PathValue("id")
	if err := h.st.RevokeTask(ctx, taskID, user.ID); err != nil {
		if err == store.ErrNotFound {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "task not found or not active")
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not revoke task")
		return
	}
	if err := h.st.DeleteChainFactsByTask(ctx, taskID); err != nil {
		h.logger.WarnContext(ctx, "chain facts cleanup failed", "err", err, "task_id", taskID)
	}

	h.publishTasksAndQueue(user.ID)

	writeJSON(w, http.StatusOK, map[string]any{
		"task_id": taskID,
		"status":  "revoked",
	})
}

// serviceActivated checks whether a service (with alias) has been activated.
// Credential-free services check service_meta; credential-backed services check the vault.
// It requires an exact alias match — callers should use serviceNotActivatedResponse
// to produce a helpful error listing available connections when this returns false.
// validateLocalService checks that a local.* service is real, enabled for the
// user, and (when action is not "*") that the requested action exists.
// Returns nil if the provider is not configured (self-hosted mode where all local
// services are allowed) or if the service and action are found in the active list.
func (h *TasksHandler) validateLocalService(ctx context.Context, userID, serviceType, action string) error {
	if h.localSvcProvider == nil {
		return nil // no provider — skip validation (self-hosted)
	}
	active, err := h.localSvcProvider.ActiveLocalServices(ctx, userID)
	if err != nil {
		return fmt.Errorf("unable to verify local service availability")
	}
	// serviceType is "local.<service_id>"; strip the prefix.
	svcID := strings.TrimPrefix(serviceType, "local.")
	for _, svc := range active {
		if svc.ServiceID == svcID {
			// Service found — validate action if not wildcard.
			if action == "*" || action == "" {
				return nil
			}
			for _, a := range svc.Actions {
				if a.ID == action {
					return nil
				}
			}
			available := make([]string, len(svc.Actions))
			for i, a := range svc.Actions {
				available[i] = a.ID
			}
			return fmt.Errorf("local service %q does not support action %q — available actions: %s",
				serviceType, action, strings.Join(available, ", "))
		}
	}
	return fmt.Errorf("local service %q is not enabled or does not exist — enable it from the Services page", serviceType)
}

func (h *TasksHandler) serviceActivated(ctx context.Context, userID, serviceType, alias string, adapter adapters.Adapter) bool {
	if adapter.ValidateCredential(nil) == nil {
		_, err := h.st.GetServiceMeta(ctx, userID, serviceType, alias)
		return err == nil
	}
	vKey := h.adapterReg.VaultKeyWithAliasForUser(serviceType, alias, userID)
	_, err := h.vault.Get(ctx, userID, vKey)
	return err == nil
}

// missingCredentialScopes loads the vault credential for a service and returns
// any required OAuth scopes that are not present for the given action. When
// the adapter implements ActionScoper, only the scopes needed by the specific
// action are checked. Otherwise falls back to all adapter RequiredScopes.
// Returns nil when the credential is missing (already caught by serviceActivated)
// or when scope data is not trustworthy (legacy credentials).
func (h *TasksHandler) missingCredentialScopes(ctx context.Context, userID, serviceType, alias, action string, adapter adapters.Adapter) []string {
	// Determine which scopes to check: per-action if available, else all.
	var required []string
	if scoper, ok := adapter.(adapters.ActionScoper); ok && action != "*" {
		required = scoper.ScopesForAction(action)
	}
	if len(required) == 0 {
		// Wildcard action or adapter doesn't implement ActionScoper —
		// skip the check rather than requiring all scopes, which would
		// over-reject tasks that only use a subset of actions.
		if action == "*" {
			return nil
		}
		required = adapter.RequiredScopes()
	}
	if len(required) == 0 {
		return nil
	}
	vKey := h.adapterReg.VaultKeyWithAliasForUser(serviceType, alias, userID)
	credBytes, err := h.vault.Get(ctx, userID, vKey)
	if err != nil || len(credBytes) == 0 {
		return nil
	}
	cred, err := credential.Parse(credBytes)
	if err != nil {
		return nil
	}
	// Only check scopes when we know they reflect what the user actually
	// granted (set by the new OAuthCallback path that reads token.Extra("scope")).
	// Legacy credentials stored the *requested* scopes which may not match
	// current RequiredScopes if new scopes were added after the credential
	// was stored.
	if !cred.ScopesGranted {
		return nil
	}
	return credential.MissingScopes(cred.Scopes, required)
}

// ── Task scope check helper ───────────────────────────────────────────────────

// TaskScopeMatch describes whether a service/action is in a task's authorized actions.
type TaskScopeMatch struct {
	InScope       bool
	AutoExecute   bool
	MatchedAction *store.TaskAction
}

// CheckTaskScope checks if service/action is in the task's authorized actions.
// It matches both exact (with alias, e.g. "google.gmail:personal") and
// base service type (e.g. "google.gmail" matches any alias).
func CheckTaskScope(task *store.Task, serviceType, alias, action string) TaskScopeMatch {
	fullService := serviceType
	if alias != "" && alias != "default" {
		fullService = serviceType + ":" + alias
	}
	// First pass: look for an exact match on the full service (including alias).
	for i := range task.AuthorizedActions {
		a := &task.AuthorizedActions[i]
		if a.Service == fullService && (a.Action == action || a.Action == "*") {
			return TaskScopeMatch{InScope: true, AutoExecute: a.AutoExecute, MatchedAction: a}
		}
	}
	// Second pass: fall back to base service type only when the request
	// didn't include an alias or no exact match was found.
	if fullService != serviceType {
		for i := range task.AuthorizedActions {
			a := &task.AuthorizedActions[i]
			if a.Service == serviceType && (a.Action == action || a.Action == "*") {
				return TaskScopeMatch{InScope: true, AutoExecute: a.AutoExecute, MatchedAction: a}
			}
		}
	}
	return TaskScopeMatch{InScope: false}
}
