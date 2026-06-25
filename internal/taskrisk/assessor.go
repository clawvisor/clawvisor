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
	runtimetasks "github.com/clawvisor/clawvisor/internal/runtime/tasks"
	"github.com/clawvisor/clawvisor/pkg/adapters"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// Assessor evaluates the risk profile of a task at creation time.
type Assessor interface {
	Assess(ctx context.Context, req AssessRequest) (*RiskAssessment, error)
}

// AssessRequest contains the data needed for task risk assessment.
//
// A request carries one of two scope shapes — the legacy v1 fields
// (AuthorizedActions, PlannedCalls) or the v2 runtime envelope
// (ExpectedTools, ExpectedEgress, RequiredCredentials, IntentVerificationMode,
// ExpectedUse). The prompt renderer handles either shape so the same
// LLMAssessor covers dashboard, control-plane, and lite-proxy task
// creation paths.
type AssessRequest struct {
	Purpose           string
	AuthorizedActions []store.TaskAction
	PlannedCalls      []store.PlannedCall
	AgentName         string
	// UserID scopes the action-context lookup so per-user MCP tool sets
	// (discovered at activation) appear in the prompt. Empty falls back to
	// the global registry only.
	UserID string

	// V2 runtime envelope. Proxy-lite tasks (and any v2-schema dashboard
	// task) declare scope here instead of AuthorizedActions/PlannedCalls.
	ExpectedTools          []runtimetasks.ExpectedTool
	ExpectedEgress         []runtimetasks.ExpectedEgress
	RequiredCredentials    []runtimetasks.RequiredCredential
	IntentVerificationMode string
	ExpectedUse            string

	// RecentUserTurns carries the human-authored chat turns leading up to
	// this task creation. When non-empty, the assessor emits an
	// IntentMatch verdict reporting whether the user's prior message(s)
	// unambiguously authorize the requested scope. Used by the
	// conversation-based auto-approval gate: a "yes" verdict paired with
	// a risk level at or below the user's configured threshold skips the
	// human approval prompt. Treated as UNTRUSTED text (may contain
	// injection); the assessor evaluates it only as data.
	RecentUserTurns []string
}

// HasEnvelope reports whether the request carries v2 envelope fields.
func (r AssessRequest) HasEnvelope() bool {
	return len(r.ExpectedTools) > 0 || len(r.ExpectedEgress) > 0 || len(r.RequiredCredentials) > 0
}

// RiskAssessment is the result of a task risk evaluation.
type RiskAssessment struct {
	RiskLevel   string           `json:"risk_level"`   // "low" | "medium" | "high" | "critical"
	Explanation string           `json:"explanation"`   // 1-2 sentence summary
	Factors     []string         `json:"factors"`       // individual risk signals
	Conflicts   []ConflictDetail `json:"conflicts"`     // internal inconsistencies within the task
	Model       string           `json:"model"`
	LatencyMS   int              `json:"latency_ms"`

	// IntentMatch reports whether the user's recent chat turns
	// unambiguously authorize the requested scope. Set only when
	// RecentUserTurns was provided to the assessor; "unknown" otherwise.
	// Values: "yes" | "partial" | "no" | "unknown".
	IntentMatch string `json:"intent_match,omitempty"`
	// IntentMatchExplanation is a 1-sentence plain-language rationale
	// surfaced to the auto-approval gate's audit trail.
	IntentMatchExplanation string `json:"intent_match_explanation,omitempty"`
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

	// geminiCacheNameFn returns the current Gemini cachedContents resource
	// name, or "" when no cache is registered. Set via StartGeminiCache.
	// Attached to every per-call llm.Client so the cache is referenced on
	// Gemini provider requests.
	geminiCacheNameFn func() string
	// geminiCacheInvalidator drops the in-process cache name and triggers
	// an async refresh when a server-side cache reference fails. Wired
	// alongside geminiCacheNameFn when a manager is in use.
	geminiCacheInvalidator func(string)
	// geminiCacheMgr owns the cache lifecycle when StartGeminiCache was
	// used. Nil when no cache was set up.
	geminiCacheMgr *llm.GeminiCacheManager
}

// NewLLMAssessor creates an LLM-backed task risk assessor.
// The registry is used to read action metadata from adapters that implement MetadataProvider.
func NewLLMAssessor(health *llm.Health, registry *adapters.Registry, logger *slog.Logger) *LLMAssessor {
	return &LLMAssessor{health: health, registry: registry, logger: logger}
}

// StartGeminiCache initializes the Gemini explicit context cache for the
// assessor's system prompt and registers it so per-request clients reference
// it automatically. cfg.SystemPrompt is filled in by the assessor and should
// be left empty by callers. On creation failure the assessor proceeds
// without caching (slower, but functional).
func (a *LLMAssessor) StartGeminiCache(ctx context.Context, cfg llm.GeminiCacheManagerConfig) error {
	if cfg.Logger == nil {
		cfg.Logger = a.logger
	}
	mgr, nameFn, invalidator, err := llm.StartCachedSystemPrompt(ctx, cfg, riskAssessmentSystemPrompt)
	if err != nil {
		return fmt.Errorf("assessor gemini cache: %w", err)
	}
	a.geminiCacheMgr = mgr
	a.geminiCacheNameFn = nameFn
	a.geminiCacheInvalidator = invalidator
	return nil
}

func (a *LLMAssessor) Assess(ctx context.Context, req AssessRequest) (*RiskAssessment, error) {
	cfg := a.health.TaskRiskConfig()
	if !cfg.Enabled {
		return nil, nil
	}

	start := time.Now()

	actionContext := buildActionContextFromRegistry(ctx, a.registry, req.UserID)
	client := llm.NewClient(cfg.LLMProviderConfig)
	if a.geminiCacheNameFn != nil {
		client.AttachGeminiCacheNameFn(a.geminiCacheNameFn)
		if a.geminiCacheInvalidator != nil {
			client.AttachGeminiCacheInvalidator(a.geminiCacheInvalidator)
		}
	}
	verificationEnabled := a.health.VerificationConfig().Enabled
	userMsg := buildAssessUserMessage(req, verificationEnabled, actionContext)
	// The system prompt is fully static — the per-user action context lives
	// in the user message. That means the Anthropic prompt-cache prefix is
	// shared across all users (and all assessments), and the Gemini
	// explicit-content cache (when configured) covers it in a single
	// cachedContents resource.
	messages := []llm.ChatMessage{
		{Role: "system", Content: riskAssessmentSystemPrompt, CacheControl: true},
		{Role: "user", Content: userMsg},
	}

	var lastErr error
	for attempt := range 2 {
		raw, usage, err := client.CompleteWithUsage(ctx, messages)
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
		llm.LogUsage(a.logger, "task_risk_assessment", cfg.Model, usage)
		return assessment, nil
	}

	a.logger.WarnContext(ctx, "task risk assessment failed after retry", "error", lastErr)
	return &RiskAssessment{
		RiskLevel:   "unknown",
		Explanation: "Risk assessment temporarily unavailable.",
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

// MergeAssessments returns the higher-of-two combination of two risk
// assessments. The level is the higher rank (see HighestRiskLevel);
// when the secondary's level outranks the primary's, the secondary's
// Explanation surfaces so the reviewer sees the binding reason.
// Factors and Conflicts always accumulate. Model and LatencyMS fall
// back to the secondary when the primary is unset so a deterministic
// floor doesn't leave the metadata empty.
//
// Canonical home for the "higher wins" merge rule both the task
// creation flow (handlers) and the expansion flow (handlers +
// llmproxy intercept) need. Lives in taskrisk because both callers
// already import this package; keeping it here is what prevents
// silent drift between the create-time and expand-time scoring.
func MergeAssessments(primary, secondary *RiskAssessment) *RiskAssessment {
	if primary == nil {
		return secondary
	}
	if secondary == nil {
		return primary
	}
	out := *primary
	out.RiskLevel = HighestRiskLevel(primary.RiskLevel, secondary.RiskLevel)
	out.Factors = append(append([]string{}, primary.Factors...), secondary.Factors...)
	out.Conflicts = append(append([]ConflictDetail{}, primary.Conflicts...), secondary.Conflicts...)
	if secondary.Explanation != "" && HighestRiskLevel(primary.RiskLevel, secondary.RiskLevel) == secondary.RiskLevel {
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

// HighestRiskLevel ranks two risk-level strings and returns the
// higher one. Ordering: "" < "unknown" < "low" < "medium" < "high" <
// "critical". The empty / unknown sentinels rank below any named
// level so an unconfigured assessor never wins against a real
// deterministic floor.
func HighestRiskLevel(a, b string) string {
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

// UnmarshalAssessment is the inverse of MarshalAssessment. Returns nil
// on empty input or parse failure — callers treat both as "no cached
// assessment" and fall back to a fresh deterministic-only read rather
// than failing the approve. The expand cache stores nothing on legacy
// rows or when the assessor was unconfigured at expand time, so an
// empty input here is the expected steady state for that path.
func UnmarshalAssessment(raw json.RawMessage) *RiskAssessment {
	if len(raw) == 0 {
		return nil
	}
	var out RiskAssessment
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	if strings.TrimSpace(out.RiskLevel) == "" {
		return nil
	}
	return &out
}

// parseRiskResponse parses the LLM response into a RiskAssessment.
func parseRiskResponse(raw string) (*RiskAssessment, error) {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(raw, "```json")
	raw = strings.TrimPrefix(raw, "```")
	raw = strings.TrimSuffix(raw, "```")
	raw = strings.TrimSpace(raw)

	var out struct {
		RiskLevel              string           `json:"risk_level"`
		Explanation            string           `json:"explanation"`
		Factors                []string         `json:"factors"`
		Conflicts              []ConflictDetail `json:"conflicts"`
		IntentMatch            string           `json:"intent_match"`
		IntentMatchExplanation string           `json:"intent_match_explanation"`
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

	// intent_match is optional in the response (legacy v1 prompts and
	// envelope-only requests without conversation context don't emit
	// it). When present, it must be one of the documented values; an
	// unrecognized value collapses to "unknown" rather than failing the
	// whole parse — the surrounding risk read is still useful.
	intentMatch := strings.ToLower(strings.TrimSpace(out.IntentMatch))
	validIntent := map[string]bool{
		"yes": true, "partial": true, "no": true, "unknown": true,
	}
	if intentMatch == "" || !validIntent[intentMatch] {
		intentMatch = "unknown"
	}

	return &RiskAssessment{
		RiskLevel:              out.RiskLevel,
		Explanation:            out.Explanation,
		Factors:                out.Factors,
		Conflicts:              out.Conflicts,
		IntentMatch:            intentMatch,
		IntentMatchExplanation: strings.TrimSpace(out.IntentMatchExplanation),
	}, nil
}
