package policy

import (
	"context"
	"testing"

	"github.com/clawvisor/clawvisor/pkg/store"
)

func TestRuntimeContextJudgeDoesNotAutoMatchActiveBinding(t *testing.T) {
	task := &store.Task{ID: "task-active", Purpose: "Bound task"}
	judge := NewLLMRuntimeContextJudge(nil, nil)
	got, err := judge.Judge(context.Background(), RuntimeContextJudgeRequest{
		SessionID:         "session-1",
		AgentID:           "agent-1",
		ActionKind:        "egress",
		Method:            "GET",
		Host:              "api.example.com",
		Path:              "/messages",
		ActiveTaskBinding: task,
		CandidateTasks:    []*store.Task{task},
	})
	if err != nil {
		t.Fatalf("Judge: %v", err)
	}
	if got.Kind != ClassificationOneOff || got.MatchedTask != nil {
		t.Fatalf("unexpected judgment: %+v", got)
	}
}

func TestRuntimeContextJudgeFallsBackToOneOffForReadLikeActions(t *testing.T) {
	judge := NewLLMRuntimeContextJudge(nil, nil)
	got, err := judge.Judge(context.Background(), RuntimeContextJudgeRequest{
		SessionID:      "session-1",
		AgentID:        "agent-1",
		ActionKind:     "egress",
		Method:         "GET",
		Host:           "api.example.com",
		Path:           "/messages",
		CandidateTasks: []*store.Task{{ID: "task-other"}},
	})
	if err != nil {
		t.Fatalf("Judge: %v", err)
	}
	if got.Kind != ClassificationOneOff || got.ResolutionHint != "allow_once" {
		t.Fatalf("unexpected judgment: %+v", got)
	}
}

func TestRuntimeContextJudgeFallsBackToNeedsNewTaskForMutatingActions(t *testing.T) {
	judge := NewLLMRuntimeContextJudge(nil, nil)
	got, err := judge.Judge(context.Background(), RuntimeContextJudgeRequest{
		SessionID:      "session-1",
		AgentID:        "agent-1",
		ActionKind:     "egress",
		Method:         "POST",
		Host:           "api.example.com",
		Path:           "/tickets",
		CandidateTasks: []*store.Task{{ID: "task-other"}},
	})
	if err != nil {
		t.Fatalf("Judge: %v", err)
	}
	if got.Kind != ClassificationNeedsNewTask || got.ResolutionHint != "allow_session" {
		t.Fatalf("unexpected judgment: %+v", got)
	}
}

func TestRuntimeContextJudgeHeuristicallyPromotesMutatingFileTool(t *testing.T) {
	judge := NewLLMRuntimeContextJudge(nil, nil)
	got, err := judge.Judge(context.Background(), RuntimeContextJudgeRequest{
		SessionID:  "session-1",
		AgentID:    "agent-1",
		ActionKind: "tool_use",
		ToolName:   "Write",
		ToolInput:  map[string]any{"file_path": "/Users/demo/hello.py"},
	})
	if err != nil {
		t.Fatalf("Judge: %v", err)
	}
	if got.Kind != ClassificationNeedsNewTask || got.ResolutionHint != "allow_session" {
		t.Fatalf("unexpected judgment: %+v", got)
	}
	if got.Confidence != "high" {
		t.Fatalf("expected high-confidence mutating heuristic, got %+v", got)
	}
}

func TestRuntimeContextJudgeFallsBackToAmbiguousForMultipleCandidates(t *testing.T) {
	judge := NewLLMRuntimeContextJudge(nil, nil)
	got, err := judge.Judge(context.Background(), RuntimeContextJudgeRequest{
		SessionID:  "session-1",
		AgentID:    "agent-1",
		ActionKind: "tool_use",
		ToolName:   "Bash",
		CandidateTasks: []*store.Task{
			{ID: "task-a", Purpose: "A"},
			{ID: "task-b", Purpose: "B"},
		},
	})
	if err != nil {
		t.Fatalf("Judge: %v", err)
	}
	if got.Kind != ClassificationAmbiguous {
		t.Fatalf("unexpected judgment: %+v", got)
	}
}
