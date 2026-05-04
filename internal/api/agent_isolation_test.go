package api_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"
)

// TestGateway_TaskRejectsCrossAgent ensures that when a task is created by
// agent A under user U, agent B (also under user U) cannot use that task to
// authorize gateway calls. This protects against cross-agent scope reuse:
// a low-trust agent should not be able to ride on a higher-trust peer's
// approved scope just because they share an owner.
func TestGateway_TaskRejectsCrossAgent(t *testing.T) {
	adapter := newMockAdapter("mock.echo", "echo").
		withResult("ok", map[string]any{"msg": "hello"})
	env := newTestEnv(t, adapter)

	sc := newScenario(t, env, "agent-a")
	if err := env.Vault.Set(context.Background(), sc.session.UserID, "mock.echo", []byte("cred")); err != nil {
		t.Fatalf("vault seed: %v", err)
	}

	taskID := sc.createApprovedTask(t, env, "mock.echo", "echo", true)

	// Same user creates a second agent and tries to call against agent A's task.
	resp := sc.session.do("POST", "/api/agents", map[string]any{"name": "agent-b"})
	body := mustStatus(t, resp, http.StatusCreated)
	agentBToken := str(t, body, "token")

	reqID := fmt.Sprintf("xagent-%s", randSuffix())
	resp = env.do("POST", "/api/gateway/request", agentBToken, map[string]any{
		"service":    "mock.echo",
		"action":     "echo",
		"params":     map[string]any{"msg": "hi"},
		"reason":     "cross-agent attempt",
		"request_id": reqID,
		"task_id":    taskID,
	})
	got := mustStatus(t, resp, http.StatusForbidden)
	if got["code"] != "FORBIDDEN" {
		t.Fatalf("expected code=FORBIDDEN, got %v (full: %v)", got["code"], got)
	}
}
