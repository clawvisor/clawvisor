package policy

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/clawvisor/clawvisor/internal/llm"
	"github.com/clawvisor/clawvisor/pkg/store"
)

type LLMGatewayRequestResolver struct {
	health *llm.Health
	logger *slog.Logger
}

func NewLLMGatewayRequestResolver(health *llm.Health, logger *slog.Logger) *LLMGatewayRequestResolver {
	return &LLMGatewayRequestResolver{health: health, logger: logger}
}

func (r *LLMGatewayRequestResolver) Resolve(ctx context.Context, req GatewayRequestResolutionRequest) (GatewayRequestClassification, error) {
	if r == nil || r.health == nil {
		return req.Classification, nil
	}
	cfg := r.health.VerificationConfig()
	if !cfg.Enabled || cfg.Endpoint == "" || cfg.Model == "" {
		return req.Classification, nil
	}

	options := resolverOptions(req.Classification)
	if len(options) == 0 {
		return req.Classification, nil
	}

	client := llm.NewClient(cfg.LLMProviderConfig).WithMaxTokens(300)
	raw, err := client.Complete(ctx, []llm.ChatMessage{
		{Role: "system", Content: gatewayRequestResolverSystemPrompt},
		{Role: "user", Content: buildGatewayRequestResolverPrompt(req, options)},
	})
	if err != nil {
		return req.Classification, err
	}

	decision, err := parseGatewayRequestResolverResponse(raw)
	if err != nil {
		return req.Classification, err
	}

	return applyResolverDecision(req.Classification, decision), nil
}

type gatewayRequestResolutionDecision struct {
	Kind   string `json:"kind"`
	TaskID string `json:"task_id,omitempty"`
}

func parseGatewayRequestResolverResponse(raw string) (gatewayRequestResolutionDecision, error) {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(raw, "```json")
	raw = strings.TrimPrefix(raw, "```")
	raw = strings.TrimSuffix(raw, "```")
	raw = strings.TrimSpace(raw)

	var out gatewayRequestResolutionDecision
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return gatewayRequestResolutionDecision{}, fmt.Errorf("parse gateway request resolver response: %w", err)
	}
	return out, nil
}

func applyResolverDecision(classification GatewayRequestClassification, decision gatewayRequestResolutionDecision) GatewayRequestClassification {
	validKinds := map[string]bool{
		ClassificationBelongsToExistingTask: true,
		ClassificationNeedsNewTask:          true,
		ClassificationOneOff:                true,
		ClassificationAmbiguous:             true,
	}
	if !validKinds[decision.Kind] {
		return classification
	}

	switch classification.Kind {
	case ClassificationAmbiguous:
		if decision.Kind == ClassificationBelongsToExistingTask {
			for _, task := range classification.CandidateTasks {
				if task != nil && task.ID == decision.TaskID {
					return GatewayRequestClassification{
						Kind:        ClassificationBelongsToExistingTask,
						MatchedTask: task,
					}
				}
			}
		}
		if decision.Kind == ClassificationAmbiguous {
			return classification
		}
	case ClassificationNeedsNewTask:
		if decision.Kind == ClassificationNeedsNewTask || decision.Kind == ClassificationOneOff {
			classification.Kind = decision.Kind
			return classification
		}
	}
	return classification
}

func resolverOptions(classification GatewayRequestClassification) []string {
	switch classification.Kind {
	case ClassificationAmbiguous:
		options := []string{ClassificationAmbiguous}
		for _, task := range classification.CandidateTasks {
			if task != nil && task.ID != "" {
				options = append(options, ClassificationBelongsToExistingTask+":"+task.ID)
			}
		}
		return options
	case ClassificationNeedsNewTask:
		return []string{ClassificationNeedsNewTask, ClassificationOneOff}
	default:
		return nil
	}
}

func buildGatewayRequestResolverPrompt(req GatewayRequestResolutionRequest, options []string) string {
	var candidateLines []string
	for _, task := range req.Classification.CandidateTasks {
		if task == nil {
			continue
		}
		candidateLines = append(candidateLines, fmt.Sprintf("- task_id=%s purpose=%q status=%s lifetime=%s", task.ID, task.Purpose, task.Status, task.Lifetime))
	}
	paramsJSON, _ := json.Marshal(req.Params)

	return fmt.Sprintf(`Request:
- service: %s
- alias: %s
- action: %s
- reason: %s
- params: %s

Deterministic classification: %s
Allowed options:
%s

Candidate tasks:
%s

Choose only among the allowed options. If you choose belongs_to_existing_task, return the matching task_id. If you are not confident, choose ambiguous. If no active task covers the request and it looks like a small ad-hoc action, choose one_off; otherwise choose needs_new_task.`,
		req.ServiceType,
		req.ServiceAlias,
		req.Action,
		req.Reason,
		string(paramsJSON),
		req.Classification.Kind,
		strings.Join(options, "\n"),
		strings.Join(candidateLines, "\n"),
	)
}

const gatewayRequestResolverSystemPrompt = `You classify uncovered Clawvisor gateway requests.

Rules:
- You may only choose among the deterministic options provided.
- Never invent authorization or a task that is not listed.
- Use the request reason and candidate task purposes to decide whether the request belongs to one active task, is ambiguous, is a one-off action, or needs a new task.
- Return strict JSON only:
  {"kind":"belongs_to_existing_task","task_id":"<task-id>"}
  {"kind":"ambiguous"}
  {"kind":"one_off"}
  {"kind":"needs_new_task"}`

func summarizeTaskCandidate(task *store.Task) string {
	if task == nil {
		return ""
	}
	return fmt.Sprintf("%s:%s", task.ID, task.Purpose)
}
