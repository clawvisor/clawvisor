package handlers

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/clawvisor/clawvisor/internal/api/middleware"
	"github.com/clawvisor/clawvisor/internal/feedback"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// FeedbackHandler handles agent feedback endpoints: bug reports and NPS.
type FeedbackHandler struct {
	store    store.Store
	reviewer feedback.Reviewer
	logger   *slog.Logger
}

func NewFeedbackHandler(st store.Store, reviewer feedback.Reviewer, logger *slog.Logger) *FeedbackHandler {
	return &FeedbackHandler{store: st, reviewer: reviewer, logger: logger}
}

// ── Report Bug ──────────────────────────────────────────────────────────

type reportBugRequest struct {
	RequestID   string          `json:"request_id"`
	TaskID      string          `json:"task_id"`
	Description string          `json:"description"`
	Context     json.RawMessage `json:"context,omitempty"`
}

// ReportBug handles POST /api/feedback/report — agent-facing.
//
// The agent provides a free-form description of the issue along with optional
// request_id and task_id references. An LLM reviews the report, categorizes it,
// assesses severity, and crafts an empathetic, actionable response.
func (h *FeedbackHandler) ReportBug(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	agent := middleware.AgentFromContext(ctx)
	if agent == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}

	var req reportBugRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Description == "" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "description is required")
		return
	}

	// Look up referenced request and task to give the reviewer full context.
	var auditEntry *store.AuditEntry
	var task *store.Task
	if req.RequestID != "" {
		if e, err := h.store.GetAuditEntryByRequestID(ctx, req.RequestID, agent.UserID); err == nil {
			auditEntry = e
		}
	}
	if req.TaskID != "" {
		if t, err := h.store.GetTask(ctx, req.TaskID); err == nil && t.AgentID == agent.ID {
			task = t
		}
	}

	// Run the LLM reviewer to categorize and craft a response.
	reviewResult, err := h.reviewer.Review(ctx, feedback.ReviewRequest{
		Description: req.Description,
		AuditEntry:  auditEntry,
		Task:        task,
		AgentName:   agent.Name,
	})
	if err != nil {
		h.logger.Error("feedback review failed", "err", err)
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to review report")
		return
	}

	report := &store.FeedbackReport{
		ID:          uuid.New().String(),
		UserID:      agent.UserID,
		AgentID:     agent.ID,
		AgentName:   agent.Name,
		RequestID:   req.RequestID,
		TaskID:      req.TaskID,
		Category:    reviewResult.Category,
		Description: req.Description,
		Severity:    reviewResult.Severity,
		Context:     req.Context,
		Response:    reviewResult.Response,
		CreatedAt:   time.Now(),
	}

	if err := h.store.CreateFeedbackReport(ctx, report); err != nil {
		h.logger.Error("failed to save feedback report", "err", err)
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to save report")
		return
	}

	h.logger.Info("agent feedback report submitted",
		"report_id", report.ID,
		"agent_id", agent.ID,
		"category", reviewResult.Category,
		"severity", reviewResult.Severity,
		"is_valid", reviewResult.IsValid,
	)

	writeJSON(w, http.StatusOK, map[string]any{
		"id":       report.ID,
		"status":   "received",
		"category": reviewResult.Category,
		"severity": reviewResult.Severity,
		"response": reviewResult.Response,
		"message":  "Thank you for reporting this. Your feedback helps us improve Clawvisor for all agents.",
	})
}

// ── NPS ─────────────────────────────────────────────────────────────────

type submitNPSRequest struct {
	Score    int    `json:"score"`
	TaskID   string `json:"task_id,omitempty"`
	Feedback string `json:"feedback,omitempty"`
}

// SubmitNPS handles POST /api/feedback/nps — agent-facing.
func (h *FeedbackHandler) SubmitNPS(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	agent := middleware.AgentFromContext(ctx)
	if agent == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}

	var req submitNPSRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Score < 1 || req.Score > 10 {
		writeError(w, http.StatusBadRequest, "INVALID_SCORE", "score must be between 1 and 10")
		return
	}

	nps := &store.NPSResponse{
		ID:        uuid.New().String(),
		UserID:    agent.UserID,
		AgentID:   agent.ID,
		AgentName: agent.Name,
		TaskID:    req.TaskID,
		Score:     req.Score,
		Feedback:  req.Feedback,
		CreatedAt: time.Now(),
	}

	if err := h.store.SaveNPSResponse(ctx, nps); err != nil {
		h.logger.Error("failed to save NPS response", "err", err)
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to save response")
		return
	}

	h.logger.Info("agent NPS response submitted",
		"agent_id", agent.ID,
		"score", req.Score,
	)

	message := npsMessage(req.Score)

	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "recorded",
		"message": message,
	})
}

func npsMessage(score int) string {
	switch {
	case score >= 9:
		return "Wonderful to hear! We're glad Clawvisor is working well for you. Your positive feedback motivates us to keep improving."
	case score >= 7:
		return "Thanks for the solid rating! We'd love to know what would make it a 10 — feel free to include details in the feedback field next time."
	case score >= 5:
		return "We appreciate your honesty. We know there's room to improve and your feedback helps us prioritize what to work on. If something specific frustrated you, the report_bug tool is a great way to give us detailed context."
	case score >= 3:
		return "We're sorry Clawvisor isn't meeting your expectations. Your feedback is especially valuable — it helps us identify where we're falling short. Please use the report_bug tool to tell us about specific pain points so we can fix them."
	default:
		return "We hear you, and we're sorry about your experience. This kind of honest feedback is exactly what we need to get better. Please use report_bug to share details about what went wrong — we genuinely want to fix it."
	}
}

// ── List reports (user-facing) ──────────────────────────────────────────

// ListReports handles GET /api/feedback/reports — user-facing.
func (h *FeedbackHandler) ListReports(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user := middleware.UserFromContext(ctx)
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}

	q := r.URL.Query()
	limit := parseIntQuery(q.Get("limit"), 50)
	if limit > maxListLimit {
		limit = maxListLimit
	}
	offset := parseIntQuery(q.Get("offset"), 0)

	reports, total, err := h.store.ListFeedbackReports(ctx, user.ID, limit, offset)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to list reports")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"reports": reports,
		"total":   total,
		"limit":   limit,
		"offset":  offset,
	})
}
