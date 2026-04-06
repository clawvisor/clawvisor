// Package taskrisk provides LLM-powered risk assessment for task scopes.
// It evaluates the risk profile of a task at creation time — scope breadth,
// purpose-scope alignment, and internal coherence — and returns a structured
// risk level with explanatory detail.
package taskrisk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/clawvisor/clawvisor/internal/llm"
	"github.com/clawvisor/clawvisor/pkg/adapters"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// Assessor evaluates the risk profile of a task at creation time.
type Assessor interface {
	Assess(ctx context.Context, req AssessRequest) (*RiskAssessment, error)
}

// AssessRequest contains the data needed for task risk assessment.
type AssessRequest struct {
	Purpose           string
	AuthorizedActions []store.TaskAction
	PlannedCalls      []store.PlannedCall
	AgentName         string
}

// RiskAssessment is the result of a task risk evaluation.
type RiskAssessment struct {
	RiskLevel   string           `json:"risk_level"`   // "low" | "medium" | "high" | "critical"
	Explanation string           `json:"explanation"`   // 1-2 sentence summary
	Factors     []string         `json:"factors"`       // individual risk signals
	Conflicts   []ConflictDetail `json:"conflicts"`     // internal inconsistencies within the task
	Model       string           `json:"model"`
	LatencyMS   int              `json:"latency_ms"`
}

// ConflictDetail describes an internal inconsistency within a task.
type ConflictDetail struct {
	Field       string `json:"field"`       // "purpose", "expected_use", "action"
	Description string `json:"description"`
	Severity    string `json:"severity"` // "info" | "warning" | "error"
}

// NoopAssessor returns nil (assessment not configured).
type NoopAssessor struct{}

func (NoopAssessor) Assess(_ context.Context, _ AssessRequest) (*RiskAssessment, error) {
	return nil, nil
}

// LLMAssessor performs task risk assessment via an LLM provider.
type LLMAssessor struct {
	health   *llm.Health
	registry *adapters.Registry
	logger   *slog.Logger
}

// NewLLMAssessor creates an LLM-backed task risk assessor.
// The registry is used to read action metadata from adapters that implement MetadataProvider.
func NewLLMAssessor(health *llm.Health, registry *adapters.Registry, logger *slog.Logger) *LLMAssessor {
	return &LLMAssessor{health: health, registry: registry, logger: logger}
}

func (a *LLMAssessor) Assess(ctx context.Context, req AssessRequest) (*RiskAssessment, error) {
	cfg := a.health.TaskRiskConfig()
	if !cfg.Enabled {
		return nil, nil
	}

	start := time.Now()

	systemPrompt := fmt.Sprintf(riskAssessmentSystemPrompt, buildActionContextFromRegistry(a.registry))
	client := llm.NewClient(cfg.LLMProviderConfig)
	userMsg := buildAssessUserMessage(req)
	messages := []llm.ChatMessage{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: userMsg},
	}

	var lastErr error
	for attempt := range 2 {
		raw, err := client.Complete(ctx, messages)
		if err != nil {
			lastErr = err
			if errors.Is(err, llm.ErrSpendCapExhausted) {
				a.health.SetSpendCapExhausted()
				break
			}
			if attempt == 0 {
				continue
			}
			break
		}

		assessment, parseErr := parseRiskResponse(raw)
		if parseErr != nil {
			lastErr = parseErr
			if attempt == 0 {
				continue
			}
			break
		}

		assessment.Model = cfg.Model
		assessment.LatencyMS = int(time.Since(start).Milliseconds())
		return assessment, nil
	}

	a.logger.Warn("task risk assessment failed after retry", "error", lastErr)
	return &RiskAssessment{
		RiskLevel:   "unknown",
		Explanation: "Risk assessment failed: " + lastErr.Error(),
		Model:       cfg.Model,
		LatencyMS:   int(time.Since(start).Milliseconds()),
	}, nil
}

// MarshalAssessment marshals a RiskAssessment to JSON for storage on the task.
func MarshalAssessment(a *RiskAssessment) json.RawMessage {
	if a == nil {
		return nil
	}
	b, err := json.Marshal(a)
	if err != nil {
		return nil
	}
	return b
}

// parseRiskResponse parses the LLM response into a RiskAssessment.
func parseRiskResponse(raw string) (*RiskAssessment, error) {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(raw, "```json")
	raw = strings.TrimPrefix(raw, "```")
	raw = strings.TrimSuffix(raw, "```")
	raw = strings.TrimSpace(raw)

	var out struct {
		RiskLevel   string           `json:"risk_level"`
		Explanation string           `json:"explanation"`
		Factors     []string         `json:"factors"`
		Conflicts   []ConflictDetail `json:"conflicts"`
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("parse risk response: %w", err)
	}

	validRiskLevel := map[string]bool{
		"low": true, "medium": true, "high": true, "critical": true,
	}
	if !validRiskLevel[out.RiskLevel] {
		return nil, fmt.Errorf("invalid risk_level: %q", out.RiskLevel)
	}

	validSeverity := map[string]bool{
		"info": true, "warning": true, "error": true,
	}
	for i, c := range out.Conflicts {
		if !validSeverity[c.Severity] {
			return nil, fmt.Errorf("invalid conflict severity at index %d: %q", i, c.Severity)
		}
	}

	return &RiskAssessment{
		RiskLevel:   out.RiskLevel,
		Explanation: out.Explanation,
		Factors:     out.Factors,
		Conflicts:   out.Conflicts,
	}, nil
}
