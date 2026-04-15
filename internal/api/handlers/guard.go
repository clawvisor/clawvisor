package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/clawvisor/clawvisor/internal/api/middleware"
	"github.com/clawvisor/clawvisor/internal/intent"
	"github.com/clawvisor/clawvisor/pkg/adapters"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// GuardHandler handles POST /api/guard/check for Claude Code permission hooks.
type GuardHandler struct {
	store      store.Store
	verifier   intent.Verifier
	adapterReg *adapters.Registry
	logger     *slog.Logger
}

// NewGuardHandler creates a GuardHandler.
func NewGuardHandler(st store.Store, verifier intent.Verifier, adapterReg *adapters.Registry, logger *slog.Logger) *GuardHandler {
	return &GuardHandler{store: st, verifier: verifier, adapterReg: adapterReg, logger: logger}
}

type guardCheckRequest struct {
	TaskID    string         `json:"task_id"`
	SessionID string         `json:"session_id"`
	ToolName  string         `json:"tool_name"`
	ToolInput map[string]any `json:"tool_input"`
}

type guardCheckResponse struct {
	Decision string `json:"decision"` // "allow" | "deny" | "ask"
	Reason   string `json:"reason,omitempty"`
}

// Check evaluates whether a Claude Code tool call should be allowed.
//
// POST /api/guard/check
// Auth: agent bearer token
func (h *GuardHandler) Check(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	start := time.Now()

	agent := middleware.AgentFromContext(ctx)
	if agent == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}

	var req guardCheckRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.ToolName == "" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "tool_name is required")
		return
	}

	service, action := mapToolToService(req.ToolName)
	reason := describeToolCall(req.ToolName, req.ToolInput)

	// Helper to log + respond in one step
	respond := func(decision, decisionReason string, taskID *string, verdict *intent.VerificationVerdict) {
		h.logGuardAudit(ctx, agent, req, service, action, reason, decision, decisionReason, taskID, verdict, start)
		resp := guardCheckResponse{Decision: decision, Reason: decisionReason}
		writeJSON(w, http.StatusOK, resp)
	}

	// ── With task_id: check task scope + intent ─────────────────────────────
	if req.TaskID != "" {
		task, err := h.store.GetTask(ctx, req.TaskID)
		if err != nil {
			writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "task not found")
			return
		}
		if task.UserID != agent.UserID {
			writeError(w, http.StatusForbidden, "FORBIDDEN", "task does not belong to this agent's user")
			return
		}
		taskID := &req.TaskID
		if task.Status != "active" {
			respond("deny", fmt.Sprintf("task is %s, not active", task.Status), taskID, nil)
			return
		}
		if task.ExpiresAt != nil && time.Now().After(*task.ExpiresAt) {
			respond("deny", "task has expired", taskID, nil)
			return
		}

		match := CheckTaskScope(task, service, "", action)

		if !match.InScope {
			respond("ask", fmt.Sprintf("tool %s (%s:%s) is not in task scope", req.ToolName, service, action), taskID, nil)
			return
		}

		if !match.AutoExecute {
			respond("ask", "tool is in scope but not auto-approved", taskID, nil)
			return
		}

		// In scope + auto_execute → run intent verification if configured
		var expectedUse string
		if match.MatchedAction != nil {
			expectedUse = match.MatchedAction.ExpectedUse
		}
		var serviceHints string
		if ada, ok := h.adapterReg.GetForUser(ctx, service, agent.UserID); ok {
			if hinter, ok := ada.(adapters.VerificationHinter); ok {
				serviceHints = hinter.VerificationHints()
			}
		}
		// Standing tasks require an explicit session_id.
		if task.Lifetime == "standing" && req.SessionID == "" {
			respond("deny", "session_id is required for standing task requests — chain context cannot be verified without it", taskID, nil)
			return
		}

		// Chain context: ephemeral tasks use task_id as implicit session;
		// standing tasks require an explicit session_id to scope facts.
		chainSessionID := req.SessionID
		if chainSessionID == "" && task.Lifetime != "standing" {
			chainSessionID = req.TaskID
		}
		var chainFacts []store.ChainFact
		if chainSessionID != "" {
			facts, _ := h.store.ListChainFacts(ctx, req.TaskID, chainSessionID, 50)
			for _, f := range facts {
				chainFacts = append(chainFacts, *f)
			}
		}

		verdict, _ := h.verifier.Verify(ctx, intent.VerifyRequest{
			TaskPurpose:        task.Purpose,
			ExpectedUse:        expectedUse,
			Service:            service,
			Action:             action,
			Params:             req.ToolInput,
			Reason:             reason,
			TaskID:             req.TaskID,
			ServiceHints:       serviceHints,
			ChainFacts:         chainFacts,
			ChainContextOptOut: false, // standing tasks without session_id are now rejected earlier
		})

		// Chain context fallback: same as gateway handler.
		if verdict != nil && !verdict.Allow && verdict.ParamScope == "violation" && len(verdict.MissingChainValues) > 0 {
			verdict = chainContextFallback(ctx, h.store, h.logger, verdict, chainFacts, req.TaskID, task, req.SessionID)
		}
		if verdict != nil && !verdict.Allow {
			respond("deny", verdict.Explanation, taskID, verdict)
			return
		}

		respond("allow", "", taskID, verdict)
		return
	}

	// ── Without task_id: check restrictions only ────────────────────────────
	restriction, _ := h.store.MatchRestriction(ctx, agent.UserID, service, action)
	if restriction != nil {
		r := restriction.Reason
		if r == "" {
			r = fmt.Sprintf("restricted: %s:%s is blocked", service, action)
		}
		respond("deny", r, nil, nil)
		return
	}

	respond("ask", "no task — deferred to user", nil, nil)
}

// logGuardAudit writes an audit log entry for a guard check decision.
func (h *GuardHandler) logGuardAudit(
	ctx context.Context,
	agent *store.Agent,
	req guardCheckRequest,
	service, action, reason, decision, decisionReason string,
	taskID *string,
	verdict *intent.VerificationVerdict,
	start time.Time,
) {
	// Map guard decisions to audit log values the frontend recognizes
	var auditDecision, auditOutcome string
	switch decision {
	case "allow":
		auditDecision = "execute"
		auditOutcome = "executed"
	case "deny":
		auditDecision = "block"
		auditOutcome = "blocked"
	case "ask":
		auditDecision = "verify"
		auditOutcome = "pending"
	}

	paramsSafe, _ := json.Marshal(map[string]any{
		"tool_name":  req.ToolName,
		"tool_input": req.ToolInput,
	})

	contextSrc := "guard"
	entry := &store.AuditEntry{
		ID:         uuid.New().String(),
		UserID:     agent.UserID,
		AgentID:    &agent.ID,
		RequestID:  uuid.New().String(),
		TaskID:     taskID,
		Timestamp:  time.Now().UTC(),
		Service:    service,
		Action:     action,
		ParamsSafe: json.RawMessage(paramsSafe),
		Decision:   auditDecision,
		Outcome:    auditOutcome,
		Reason:     nullableStr(decisionReason),
		ContextSrc: &contextSrc,
		DurationMS: int(time.Since(start).Milliseconds()),
	}

	if verdict != nil {
		entry.Verification = intent.MarshalVerdict(verdict)
	}

	if err := h.store.LogAudit(ctx, entry); err != nil {
		h.logger.Warn("guard audit log failed", "err", err)
	}

	if taskID != nil {
		_ = h.store.IncrementTaskRequestCount(ctx, *taskID)
	}
}

// mapToolToService maps a Claude Code tool name to a service:action pair.
func mapToolToService(toolName string) (service, action string) {
	switch toolName {
	case "Read":
		return "file", "read"
	case "Write":
		return "file", "write"
	case "Edit":
		return "file", "write"
	case "NotebookEdit":
		return "file", "write"
	case "Glob":
		return "search", "glob"
	case "Grep":
		return "search", "grep"
	case "Bash":
		return "bash", "execute"
	case "WebFetch":
		return "web", "fetch"
	case "WebSearch":
		return "web", "search"
	case "Task":
		return "agent", "delegate"
	default:
		return "unknown", strings.ToLower(toolName)
	}
}

// describeToolCall builds a human-readable reason string from tool input
// for intent verification (which expects a "reason" field).
func describeToolCall(toolName string, input map[string]any) string {
	switch toolName {
	case "Bash":
		cmd, _ := input["command"].(string)
		desc, _ := input["description"].(string)
		if desc != "" && cmd != "" {
			return fmt.Sprintf("execute: %s (%s)", cmd, desc)
		} else if cmd != "" {
			return fmt.Sprintf("execute: %s", cmd)
		}
	case "Read":
		if fp, ok := input["file_path"].(string); ok {
			return fmt.Sprintf("read %s", fp)
		}
	case "Write":
		if fp, ok := input["file_path"].(string); ok {
			return fmt.Sprintf("create/overwrite file %s", fp)
		}
	case "Edit":
		fp, _ := input["file_path"].(string)
		old, _ := input["old_string"].(string)
		newStr, _ := input["new_string"].(string)
		if fp != "" {
			if old != "" {
				oldSnip := truncate(old, 80)
				newSnip := truncate(newStr, 80)
				return fmt.Sprintf("edit %s: replace %q with %q", fp, oldSnip, newSnip)
			}
			return fmt.Sprintf("edit %s", fp)
		}
	case "Glob":
		if p, ok := input["pattern"].(string); ok {
			return fmt.Sprintf("glob %s", p)
		}
	case "Grep":
		if p, ok := input["pattern"].(string); ok {
			return fmt.Sprintf("grep %s", p)
		}
	case "WebFetch":
		if u, ok := input["url"].(string); ok {
			return fmt.Sprintf("fetch %s", u)
		}
	}
	return fmt.Sprintf("%s tool call", toolName)
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

// guardVirtualServices are scope-only service types used by permission hooks
// (e.g. clawvisor-guard). They never execute through adapters.
var guardVirtualServices = map[string]bool{
	"file":    true,
	"bash":    true,
	"search":  true,
	"web":     true,
	"agent":   true,
	"unknown": true,
}

// isGuardVirtualService returns true if the service type is a guard-only
// scope marker that should skip adapter validation.
func isGuardVirtualService(serviceType string) bool {
	return guardVirtualServices[serviceType]
}
