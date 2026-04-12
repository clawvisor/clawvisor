// Package feedback provides LLM-powered review of agent bug reports.
package feedback

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/clawvisor/clawvisor/internal/llm"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// ReviewRequest contains the agent's bug report plus contextual data.
type ReviewRequest struct {
	Description string              // The agent's free-form description of the issue
	AuditEntry  *store.AuditEntry   // The referenced gateway request (nil if not found)
	Task        *store.Task         // The referenced task (nil if not found)
	AgentName   string              // Name of the reporting agent
}

// ReviewResult is the LLM's assessment of the bug report.
type ReviewResult struct {
	Category    string `json:"category"`    // wrong_block | wrong_deny | slow_approval | scope_too_narrow | unclear_error | misunderstanding | feature_request | other
	Severity    string `json:"severity"`    // low | medium | high | critical
	IsValid     bool   `json:"is_valid"`    // whether the report describes a genuine issue vs. user confusion
	Response    string `json:"response"`    // empathetic, actionable response for the agent
	Summary     string `json:"summary"`     // one-line summary for internal tracking
	Model       string `json:"-"`
	LatencyMS   int    `json:"-"`
}

// Reviewer reviews agent feedback reports.
type Reviewer interface {
	Review(ctx context.Context, req ReviewRequest) (*ReviewResult, error)
}

// NoopReviewer returns a generic response when LLM is not configured.
type NoopReviewer struct{}

func (NoopReviewer) Review(_ context.Context, req ReviewRequest) (*ReviewResult, error) {
	return &ReviewResult{
		Category: "other",
		Severity: "medium",
		IsValid:  true,
		Response: "Thank you for reporting this. We've recorded your feedback and will review it to improve Clawvisor. If you're blocked, try broadening your task scope with more authorized_actions, using planned_calls to pre-register operations, or setting auto_execute: true for routine actions.",
		Summary:  "Agent feedback (LLM review unavailable)",
	}, nil
}

// LLMReviewer uses an LLM to analyze and respond to agent bug reports.
type LLMReviewer struct {
	health *llm.Health
	logger *slog.Logger
}

// NewLLMReviewer creates a new LLM-powered feedback reviewer.
func NewLLMReviewer(health *llm.Health, logger *slog.Logger) *LLMReviewer {
	return &LLMReviewer{health: health, logger: logger}
}

func (r *LLMReviewer) Review(ctx context.Context, req ReviewRequest) (*ReviewResult, error) {
	cfg := r.health.FeedbackReviewConfig()
	if !cfg.Enabled {
		return NoopReviewer{}.Review(ctx, req)
	}

	start := time.Now()
	client := llm.NewClient(cfg.LLMProviderConfig).WithMaxTokens(2048)

	userMsg := buildReviewUserMessage(req)
	messages := []llm.ChatMessage{
		{Role: "system", Content: reviewSystemPrompt},
		{Role: "user", Content: userMsg},
	}

	var lastErr error
	for attempt := range 2 {
		raw, err := client.Complete(ctx, messages)
		if err != nil {
			lastErr = err
			if errors.Is(err, llm.ErrSpendCapExhausted) {
				r.health.SetSpendCapExhausted()
				break
			}
			if attempt == 0 {
				continue
			}
			break
		}

		result, parseErr := parseReviewResponse(raw)
		if parseErr != nil {
			lastErr = parseErr
			if attempt == 0 {
				continue
			}
			break
		}

		result.Model = cfg.Model
		result.LatencyMS = int(time.Since(start).Milliseconds())
		return result, nil
	}

	r.logger.Warn("feedback review LLM call failed, using fallback", "error", lastErr)
	fallback, _ := NoopReviewer{}.Review(ctx, req)
	fallback.Model = cfg.Model
	fallback.LatencyMS = int(time.Since(start).Milliseconds())
	return fallback, nil
}

func buildReviewUserMessage(req ReviewRequest) string {
	var b strings.Builder

	fmt.Fprintf(&b, "## Agent Bug Report\n\n")
	fmt.Fprintf(&b, "**Agent:** %s\n", req.AgentName)
	fmt.Fprintf(&b, "**Report:**\n%s\n\n", req.Description)

	if req.AuditEntry != nil {
		e := req.AuditEntry
		fmt.Fprintf(&b, "## Referenced Gateway Request\n\n")
		fmt.Fprintf(&b, "- **Service:** %s\n", e.Service)
		fmt.Fprintf(&b, "- **Action:** %s\n", e.Action)
		fmt.Fprintf(&b, "- **Decision:** %s\n", e.Decision)
		fmt.Fprintf(&b, "- **Outcome:** %s\n", e.Outcome)
		fmt.Fprintf(&b, "- **Duration:** %dms\n", e.DurationMS)
		if e.Reason != nil {
			fmt.Fprintf(&b, "- **Reason given:** %s\n", *e.Reason)
		}
		if e.ErrorMsg != nil && *e.ErrorMsg != "" {
			fmt.Fprintf(&b, "- **Error:** %s\n", *e.ErrorMsg)
		}
		if len(e.Verification) > 0 && string(e.Verification) != "null" {
			fmt.Fprintf(&b, "- **Verification verdict:** %s\n", string(e.Verification))
		}
		b.WriteString("\n")
	}

	if req.Task != nil {
		t := req.Task
		fmt.Fprintf(&b, "## Referenced Task\n\n")
		fmt.Fprintf(&b, "- **Purpose:** %s\n", t.Purpose)
		fmt.Fprintf(&b, "- **Status:** %s\n", t.Status)
		fmt.Fprintf(&b, "- **Lifetime:** %s\n", t.Lifetime)
		fmt.Fprintf(&b, "- **Request count:** %d\n", t.RequestCount)
		fmt.Fprintf(&b, "- **Authorized actions:** %d\n", len(t.AuthorizedActions))
		for _, a := range t.AuthorizedActions {
			autoExec := "no"
			if a.AutoExecute {
				autoExec = "yes"
			}
			fmt.Fprintf(&b, "  - %s:%s (auto_execute: %s", a.Service, a.Action, autoExec)
			if a.ExpectedUse != "" {
				fmt.Fprintf(&b, ", expected_use: %q", a.ExpectedUse)
			}
			b.WriteString(")\n")
		}
		if t.RiskLevel != "" {
			fmt.Fprintf(&b, "- **Risk level:** %s\n", t.RiskLevel)
		}
		b.WriteString("\n")
	}

	if req.AuditEntry == nil && req.Task == nil {
		b.WriteString("(No request_id or task_id was provided — the agent did not reference a specific request or task.)\n\n")
	}

	return b.String()
}

var validCategories = map[string]bool{
	"wrong_block": true, "wrong_deny": true, "slow_approval": true,
	"scope_too_narrow": true, "unclear_error": true, "misunderstanding": true,
	"feature_request": true, "other": true,
}

var validSeverities = map[string]bool{
	"low": true, "medium": true, "high": true, "critical": true,
}

func parseReviewResponse(raw string) (*ReviewResult, error) {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(raw, "```json")
	raw = strings.TrimSuffix(raw, "```")
	raw = strings.TrimSpace(raw)

	var out ReviewResult
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("parse feedback review response: %w", err)
	}

	if !validCategories[out.Category] {
		return nil, fmt.Errorf("invalid category: %q", out.Category)
	}
	if !validSeverities[out.Severity] {
		return nil, fmt.Errorf("invalid severity: %q", out.Severity)
	}
	if out.Response == "" {
		return nil, fmt.Errorf("empty response from LLM")
	}

	return &out, nil
}
