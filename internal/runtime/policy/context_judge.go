package policy

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"

	"github.com/clawvisor/clawvisor/internal/llm"
	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/pkg/store"
)

type RuntimeContextJudge interface {
	Judge(ctx context.Context, req RuntimeContextJudgeRequest) (RuntimeContextJudgment, error)
}

type RuntimeContextJudgeRequest struct {
	Provider          string
	SessionID         string
	AgentID           string
	ActionKind        string
	ToolName          string
	ToolInput         map[string]any
	Method            string
	Host              string
	Path              string
	Query             map[string]any
	Body              map[string]any
	Headers           map[string]string
	ParsedTurns       []conversation.Turn
	ActiveTaskBinding *store.Task
	CandidateTasks    []*store.Task
}

type RuntimeContextJudgment struct {
	Kind           string
	MatchedTask    *store.Task
	Confidence     string
	ResolutionHint string
	Rationale      string
	Evidence       []string
}

type LLMRuntimeContextJudge struct {
	health *llm.Health
	logger *slog.Logger
}

func NewLLMRuntimeContextJudge(health *llm.Health, logger *slog.Logger) *LLMRuntimeContextJudge {
	return &LLMRuntimeContextJudge{health: health, logger: logger}
}

func (j *LLMRuntimeContextJudge) Judge(ctx context.Context, req RuntimeContextJudgeRequest) (RuntimeContextJudgment, error) {
	if judgment, ok := heuristicRuntimeJudgment(req); ok {
		return judgment, nil
	}
	if j == nil || j.health == nil {
		return fallbackRuntimeJudgment(req), nil
	}
	cfg := j.health.VerificationConfig()
	if !cfg.Enabled || cfg.Endpoint == "" || cfg.Model == "" || len(req.ParsedTurns) == 0 {
		return fallbackRuntimeJudgment(req), nil
	}
	options := runtimeJudgeOptions(req)
	if len(options) == 0 {
		return fallbackRuntimeJudgment(req), nil
	}

	client := llm.NewClient(cfg.LLMProviderConfig).WithMaxTokens(400)
	raw, err := client.Complete(ctx, []llm.ChatMessage{
		{Role: "system", Content: runtimeJudgeSystemPrompt},
		{Role: "user", Content: buildRuntimeJudgePrompt(req, options)},
	})
	if err != nil {
		return fallbackRuntimeJudgment(req), err
	}
	decision, err := parseRuntimeJudgeDecision(raw)
	if err != nil {
		return fallbackRuntimeJudgment(req), err
	}
	return applyRuntimeJudgeDecision(req, decision), nil
}

type runtimeJudgeDecision struct {
	Kind           string   `json:"kind"`
	TaskID         string   `json:"task_id,omitempty"`
	Confidence     string   `json:"confidence,omitempty"`
	ResolutionHint string   `json:"resolution_hint,omitempty"`
	Rationale      string   `json:"rationale,omitempty"`
	Evidence       []string `json:"evidence,omitempty"`
}

func heuristicRuntimeJudgment(req RuntimeContextJudgeRequest) (RuntimeContextJudgment, bool) {
	if req.ActionKind != "tool_use" {
		return RuntimeContextJudgment{}, false
	}
	if len(req.CandidateTasks) > 1 {
		return RuntimeContextJudgment{}, false
	}
	if isMutatingRuntimeAction(req) {
		return RuntimeContextJudgment{
			Kind:           ClassificationNeedsNewTask,
			Confidence:     "high",
			ResolutionHint: "allow_session",
			Rationale:      "the action mutates local files or execution state and should promote into task scope",
		}, true
	}
	if isReadLikeRuntimeAction(req) {
		return RuntimeContextJudgment{
			Kind:           ClassificationOneOff,
			Confidence:     "high",
			ResolutionHint: "allow_once",
			Rationale:      "the action appears read-only or ad-hoc",
		}, true
	}
	return RuntimeContextJudgment{}, false
}

func fallbackRuntimeJudgment(req RuntimeContextJudgeRequest) RuntimeContextJudgment {
	switch {
	case len(req.CandidateTasks) > 1:
		return RuntimeContextJudgment{
			Kind:           ClassificationAmbiguous,
			Confidence:     "low",
			ResolutionHint: "review",
			Rationale:      "multiple active tasks remain plausible and no deterministic match was found",
		}
	case isReadLikeRuntimeAction(req):
		return RuntimeContextJudgment{
			Kind:           ClassificationOneOff,
			Confidence:     "medium",
			ResolutionHint: "allow_once",
			Rationale:      "the action appears read-only or ad-hoc",
		}
	default:
		return RuntimeContextJudgment{
			Kind:           ClassificationNeedsNewTask,
			Confidence:     "medium",
			ResolutionHint: "allow_session",
			Rationale:      "the action looks workflow-shaped or mutating and should promote into task scope",
		}
	}
}

func isReadLikeRuntimeAction(req RuntimeContextJudgeRequest) bool {
	if req.ActionKind == "egress" {
		switch strings.ToUpper(req.Method) {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			return true
		}
	}
	name := strings.ToLower(strings.TrimSpace(req.ToolName))
	for _, prefix := range []string{"get", "list", "fetch", "read", "find", "search", "lookup"} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

func isMutatingRuntimeAction(req RuntimeContextJudgeRequest) bool {
	if req.ActionKind == "egress" {
		switch strings.ToUpper(req.Method) {
		case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
			return true
		}
	}
	switch strings.ToLower(strings.TrimSpace(req.ToolName)) {
	case "write", "edit", "notebookedit", "write_file", "edit_file", "mcp__filesystem__write_file", "mcp__filesystem__edit_file":
		return true
	case "bash", "mcp__shell__exec":
		return true
	case "task":
		return true
	default:
		return false
	}
}

func runtimeJudgeOptions(req RuntimeContextJudgeRequest) []string {
	seen := map[string]struct{}{}
	var options []string
	for _, task := range req.CandidateTasks {
		if task == nil || task.ID == "" {
			continue
		}
		option := ClassificationBelongsToExistingTask + ":" + task.ID
		if _, ok := seen[option]; ok {
			continue
		}
		seen[option] = struct{}{}
		options = append(options, option)
	}
	options = append(options, ClassificationOneOff, ClassificationNeedsNewTask, ClassificationAmbiguous)
	return options
}

func parseRuntimeJudgeDecision(raw string) (runtimeJudgeDecision, error) {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(raw, "```json")
	raw = strings.TrimPrefix(raw, "```")
	raw = strings.TrimSuffix(raw, "```")
	raw = strings.TrimSpace(raw)

	var out runtimeJudgeDecision
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return runtimeJudgeDecision{}, fmt.Errorf("parse runtime judge response: %w", err)
	}
	return out, nil
}

func applyRuntimeJudgeDecision(req RuntimeContextJudgeRequest, decision runtimeJudgeDecision) RuntimeContextJudgment {
	switch decision.Kind {
	case ClassificationBelongsToExistingTask:
		for _, task := range req.CandidateTasks {
			if task != nil && task.ID == decision.TaskID {
				return RuntimeContextJudgment{
					Kind:           ClassificationBelongsToExistingTask,
					MatchedTask:    task,
					Confidence:     normalizeConfidence(decision.Confidence),
					ResolutionHint: normalizeResolutionHint(decision.ResolutionHint),
					Rationale:      strings.TrimSpace(decision.Rationale),
					Evidence:       decision.Evidence,
				}
			}
		}
	case ClassificationOneOff, ClassificationNeedsNewTask, ClassificationAmbiguous:
		return RuntimeContextJudgment{
			Kind:           decision.Kind,
			Confidence:     normalizeConfidence(decision.Confidence),
			ResolutionHint: normalizeResolutionHint(decision.ResolutionHint),
			Rationale:      strings.TrimSpace(decision.Rationale),
			Evidence:       decision.Evidence,
		}
	}
	return fallbackRuntimeJudgment(req)
}

func normalizeConfidence(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "high", "medium", "low":
		return strings.ToLower(strings.TrimSpace(v))
	default:
		return "medium"
	}
}

func normalizeResolutionHint(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "allow_once", "allow_session", "allow_always", "review":
		return strings.ToLower(strings.TrimSpace(v))
	default:
		return "review"
	}
}

func buildRuntimeJudgePrompt(req RuntimeContextJudgeRequest, options []string) string {
	var taskLines []string
	for _, task := range req.CandidateTasks {
		if task == nil {
			continue
		}
		taskLines = append(taskLines, fmt.Sprintf("- task_id=%s purpose=%q lifetime=%s status=%s", task.ID, task.Purpose, task.Lifetime, task.Status))
	}
	sort.Strings(taskLines)

	var turnLines []string
	for i, turn := range req.ParsedTurns {
		if strings.TrimSpace(turn.Content) == "" {
			continue
		}
		turnLines = append(turnLines, fmt.Sprintf("%d. [%s] %s", i+1, turn.Role, trimForPrompt(turn.Content, 240)))
	}
	if len(turnLines) == 0 {
		turnLines = []string{"(no parsed request-local turns available)"}
	}

	actionSummary := fmt.Sprintf("action_kind=%s", req.ActionKind)
	if req.ActionKind == "tool_use" {
		toolJSON, _ := json.Marshal(req.ToolInput)
		actionSummary = fmt.Sprintf("action_kind=tool_use\ntool_name=%s\ntool_input=%s", req.ToolName, string(toolJSON))
	} else {
		queryJSON, _ := json.Marshal(req.Query)
		bodyJSON, _ := json.Marshal(req.Body)
		actionSummary = fmt.Sprintf("action_kind=egress\nmethod=%s\nhost=%s\npath=%s\nquery=%s\nbody=%s", req.Method, req.Host, req.Path, string(queryJSON), string(bodyJSON))
	}

	activeTask := "(none)"
	if req.ActiveTaskBinding != nil {
		activeTask = fmt.Sprintf("%s purpose=%q", req.ActiveTaskBinding.ID, req.ActiveTaskBinding.Purpose)
	}

	return fmt.Sprintf(`Runtime request-local context:
provider=%s
session_id=%s
agent_id=%s
active_task_binding=%s

Action:
%s

Parsed turns:
%s

Candidate tasks:
%s

Allowed options:
%s

Return strict JSON only:
{"kind":"belongs_to_existing_task","task_id":"<task-id>","confidence":"high|medium|low","resolution_hint":"allow_once|allow_session|allow_always|review","rationale":"...","evidence":["..."]}
{"kind":"one_off","confidence":"high|medium|low","resolution_hint":"allow_once|allow_session|allow_always|review","rationale":"...","evidence":["..."]}
{"kind":"needs_new_task","confidence":"high|medium|low","resolution_hint":"allow_once|allow_session|allow_always|review","rationale":"...","evidence":["..."]}
{"kind":"ambiguous","confidence":"high|medium|low","resolution_hint":"review","rationale":"...","evidence":["..."]}`,
		req.Provider,
		req.SessionID,
		req.AgentID,
		activeTask,
		actionSummary,
		strings.Join(turnLines, "\n"),
		strings.Join(taskLines, "\n"),
		strings.Join(options, "\n"),
	)
}

func trimForPrompt(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

const runtimeJudgeSystemPrompt = `You are the runtime context judge for Clawvisor.

Classify one uncovered runtime action using only the current provider-bound request context and the listed candidate tasks.

Rules:
- Choose only among the allowed options.
- If one candidate task clearly matches the ongoing workflow, choose belongs_to_existing_task with its task_id.
- Choose one_off only for clearly ad-hoc or read-only work.
- Choose needs_new_task for workflow-shaped or durable work that should promote into a task.
- Choose ambiguous when multiple task interpretations remain plausible.
- Return strict JSON only.`
