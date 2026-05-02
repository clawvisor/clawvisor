package policy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/clawvisor/clawvisor/internal/llm"
	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/pkg/config"
	"github.com/clawvisor/clawvisor/pkg/store"
)

func TestRuntimeContextJudgeUsesLLMWhenConfigured(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{
					"message": map[string]any{
						"content": `{"kind":"belongs_to_existing_task","task_id":"task-1","confidence":"high","resolution_hint":"allow_session","rationale":"the prior turns clearly continue the same investigation","evidence":["same ticket thread","same API host"]}`,
					},
				},
			},
		})
	}))
	defer ts.Close()

	cfg := config.Default()
	cfg.LLM.Verification.Enabled = true
	cfg.LLM.Verification.Endpoint = ts.URL + "/v1"
	cfg.LLM.Verification.APIKey = "test-key"
	cfg.LLM.Verification.Model = "test-model"
	health := llm.NewHealth(cfg.LLM)
	judge := NewLLMRuntimeContextJudge(health, nil)
	task := &store.Task{ID: "task-1", Purpose: "Investigate the same runtime issue"}

	got, err := judge.Judge(context.Background(), RuntimeContextJudgeRequest{
		Provider:       "openai",
		SessionID:      "session-1",
		AgentID:        "agent-1",
		ActionKind:     "egress",
		Method:         "POST",
		Host:           "api.example.com",
		Path:           "/tickets",
		ParsedTurns:    []conversation.Turn{{Role: conversation.RoleUser, Content: "continue the same ticket investigation"}},
		CandidateTasks: []*store.Task{task},
	})
	if err != nil {
		t.Fatalf("Judge: %v", err)
	}
	if got.Kind != ClassificationBelongsToExistingTask || got.MatchedTask == nil || got.MatchedTask.ID != task.ID {
		t.Fatalf("unexpected judgment: %+v", got)
	}
	if got.ResolutionHint != "allow_session" || got.Confidence != "high" {
		t.Fatalf("expected LLM decision details to survive, got %+v", got)
	}
}

func TestRuntimeContextJudgeLLMFailureFallsBackCleanly(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer ts.Close()

	cfg := config.Default()
	cfg.LLM.Verification.Enabled = true
	cfg.LLM.Verification.Endpoint = ts.URL + "/v1"
	cfg.LLM.Verification.APIKey = "test-key"
	cfg.LLM.Verification.Model = "test-model"
	health := llm.NewHealth(cfg.LLM)
	judge := NewLLMRuntimeContextJudge(health, nil)

	got, err := judge.Judge(context.Background(), RuntimeContextJudgeRequest{
		Provider:    "openai",
		SessionID:   "session-1",
		AgentID:     "agent-1",
		ActionKind:  "egress",
		Method:      "POST",
		Host:        "api.example.com",
		Path:        "/tickets",
		ParsedTurns: []conversation.Turn{{Role: conversation.RoleUser, Content: "create the ticket for the user"}},
	})
	if err == nil {
		t.Fatal("expected LLM failure to be reported")
	}
	if got.Kind != ClassificationNeedsNewTask || got.ResolutionHint != "allow_session" {
		t.Fatalf("expected fallback mutating-action judgment, got %+v", got)
	}
}
