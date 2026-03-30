package api_test

import (
	"fmt"
	"net/http"
	"testing"
)

func TestTask_ContentDedup_IdenticalCreation(t *testing.T) {
	// Two identical task creation requests from the same agent should return
	// the same task_id — only one task should be created.
	adapter := newMockAdapter("mock.taskdedup", "run")
	env := newTestEnv(t, adapter)
	sc := newScenario(t, env, "taskdedup-ident")
	sc.activateService(t, env, "mock.taskdedup")

	body := map[string]any{
		"purpose": "dedup test task",
		"authorized_actions": []map[string]any{{
			"service": "mock.taskdedup", "action": "run", "auto_execute": true,
		}},
	}

	resp1 := env.do("POST", "/api/tasks", sc.AgentToken, body)
	b1 := mustStatus(t, resp1, http.StatusCreated)
	if b1["status"] != "pending_approval" {
		t.Fatalf("first: expected pending_approval, got %v", b1["status"])
	}
	taskID1 := str(t, b1, "task_id")

	resp2 := env.do("POST", "/api/tasks", sc.AgentToken, body)
	b2 := mustStatus(t, resp2, http.StatusCreated)
	taskID2 := str(t, b2, "task_id")

	if taskID1 != taskID2 {
		t.Errorf("expected same task_id for identical requests, got %q and %q", taskID1, taskID2)
	}

	// Only one task should exist.
	resp := sc.session.do("GET", "/api/tasks", nil)
	tasksBody := mustStatus(t, resp, http.StatusOK)
	count := 0
	for _, e := range arr(t, tasksBody, "tasks") {
		task := e.(map[string]any)
		if task["purpose"] == "dedup test task" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected 1 task, got %d", count)
	}
}

func TestTask_ContentDedup_DifferentPurpose_NotDeduped(t *testing.T) {
	// Tasks with different purposes should not be deduped.
	adapter := newMockAdapter("mock.taskdedup2", "run")
	env := newTestEnv(t, adapter)
	sc := newScenario(t, env, "taskdedup-diff")
	sc.activateService(t, env, "mock.taskdedup2")

	resp1 := env.do("POST", "/api/tasks", sc.AgentToken, map[string]any{
		"purpose": fmt.Sprintf("purpose A %s", randSuffix()),
		"authorized_actions": []map[string]any{{
			"service": "mock.taskdedup2", "action": "run", "auto_execute": true,
		}},
	})
	b1 := mustStatus(t, resp1, http.StatusCreated)

	resp2 := env.do("POST", "/api/tasks", sc.AgentToken, map[string]any{
		"purpose": fmt.Sprintf("purpose B %s", randSuffix()),
		"authorized_actions": []map[string]any{{
			"service": "mock.taskdedup2", "action": "run", "auto_execute": true,
		}},
	})
	b2 := mustStatus(t, resp2, http.StatusCreated)

	if b1["task_id"] == b2["task_id"] {
		t.Error("different purposes should produce different tasks")
	}
}

func TestTask_ContentDedup_DifferentAgents_NotDeduped(t *testing.T) {
	// Different agents creating identical tasks should not be deduped.
	adapter := newMockAdapter("mock.taskdedup3", "run")
	env := newTestEnv(t, adapter)
	sc1 := newScenario(t, env, "taskdedup-ag1")
	sc2 := newScenario(t, env, "taskdedup-ag2")
	sc1.activateService(t, env, "mock.taskdedup3")
	sc2.activateService(t, env, "mock.taskdedup3")

	body := map[string]any{
		"purpose": "shared purpose task",
		"authorized_actions": []map[string]any{{
			"service": "mock.taskdedup3", "action": "run", "auto_execute": true,
		}},
	}

	resp1 := env.do("POST", "/api/tasks", sc1.AgentToken, body)
	b1 := mustStatus(t, resp1, http.StatusCreated)

	resp2 := env.do("POST", "/api/tasks", sc2.AgentToken, body)
	b2 := mustStatus(t, resp2, http.StatusCreated)

	if b1["task_id"] == b2["task_id"] {
		t.Error("different agents should produce different tasks")
	}
}
