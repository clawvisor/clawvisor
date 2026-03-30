package api_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// ── Restrictions ──────────────────────────────────────────────────────────────

func TestRestrictions_CRUD(t *testing.T) {
	env := newTestEnv(t)
	s := newSession(t, env)

	// Create
	resp := s.do("POST", "/api/restrictions", map[string]any{
		"service": "google.gmail", "action": "send", "reason": "no sending",
	})
	body := mustStatus(t, resp, http.StatusCreated)
	id := str(t, body, "id")
	if id == "" {
		t.Fatal("create restriction: id empty")
	}

	// List
	resp = s.do("GET", "/api/restrictions", nil)
	var restrictions []any
	decode(t, resp, &restrictions)
	if len(restrictions) != 1 {
		t.Errorf("list restrictions: expected 1, got %d", len(restrictions))
	}

	// Delete
	resp = s.do("DELETE", fmt.Sprintf("/api/restrictions/%s", id), nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("delete restriction: expected 204, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// List after delete
	resp = s.do("GET", "/api/restrictions", nil)
	var after []any
	decode(t, resp, &after)
	if len(after) != 0 {
		t.Errorf("after delete: expected 0, got %d", len(after))
	}
}

func TestRestrictions_Duplicate_Conflict(t *testing.T) {
	env := newTestEnv(t)
	s := newSession(t, env)

	s.do("POST", "/api/restrictions", map[string]any{
		"service": "google.gmail", "action": "send",
	})
	resp := s.do("POST", "/api/restrictions", map[string]any{
		"service": "google.gmail", "action": "send",
	})
	mustStatus(t, resp, http.StatusConflict)
}

// ── Agents ────────────────────────────────────────────────────────────────────

func TestAgents_Create_TokenShownOnce(t *testing.T) {
	env := newTestEnv(t)
	s := newSession(t, env)

	resp := s.do("POST", "/api/agents", map[string]any{"name": "my-agent"})
	body := mustStatus(t, resp, http.StatusCreated)

	token := str(t, body, "token")
	if token == "" {
		t.Fatal("create agent: token missing")
	}
	if str(t, body, "id") == "" {
		t.Error("create agent: id missing")
	}

	// Token is NOT stored in plaintext — listing agents should NOT include it
	resp = s.do("GET", "/api/agents", nil)
	var agents []any
	decode(t, resp, &agents)
	if len(agents) != 1 {
		t.Fatalf("list agents: expected 1, got %d", len(agents))
	}
	a := agents[0].(map[string]any)
	if _, ok := a["token"]; ok {
		t.Error("list agents: raw token should not appear in list response")
	}
}

func TestAgents_RequireUserAuth(t *testing.T) {
	env := newTestEnv(t)

	resp := env.do("GET", "/api/agents", "", nil) // no token
	mustStatus(t, resp, http.StatusUnauthorized)
}

func TestAgents_Delete(t *testing.T) {
	env := newTestEnv(t)
	s := newSession(t, env)

	resp := s.do("POST", "/api/agents", map[string]any{"name": "throwaway"})
	body := mustStatus(t, resp, http.StatusCreated)
	id := str(t, body, "id")

	resp = s.do("DELETE", fmt.Sprintf("/api/agents/%s", id), nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("delete agent: expected 204, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = s.do("GET", "/api/agents", nil)
	var agents []any
	decode(t, resp, &agents)
	if len(agents) != 0 {
		t.Errorf("after delete: expected 0 agents, got %d", len(agents))
	}
}

// ── Gateway ───────────────────────────────────────────────────────────────────

func TestGateway_NoToken_Returns401(t *testing.T) {
	env := newTestEnv(t)

	resp := env.do("POST", "/api/gateway/request", "", map[string]any{
		"service": "google.gmail", "action": "send", "params": map[string]any{},
	})
	mustStatus(t, resp, http.StatusUnauthorized)
}

func TestGateway_MissingServiceAction_Returns400(t *testing.T) {
	env := newTestEnv(t)
	sc := newScenario(t, env, "bot")

	resp := env.do("POST", "/api/gateway/request", sc.AgentToken, map[string]any{
		"service": "", "action": "", "params": map[string]any{},
	})
	mustStatus(t, resp, http.StatusBadRequest)
}

func TestGateway_Block_WithRestriction(t *testing.T) {
	env := newTestEnv(t)
	sc := newScenario(t, env, "automation")

	sc.createRestriction(t, "google.gmail", "send", "Blocked by test restriction")

	result := sc.gatewayRequest(env, "req-block-1", "google.gmail", "send")
	if result["status"] != "blocked" {
		t.Errorf("expected status=blocked, got %v", result["status"])
	}
	if result["reason"] == nil || result["reason"] == "" {
		t.Error("block: reason should be populated")
	}
	if result["audit_id"] == nil {
		t.Error("block: audit_id missing")
	}
}

func TestGateway_Block_AuditEntryRecorded(t *testing.T) {
	env := newTestEnv(t)
	sc := newScenario(t, env, "automation")
	sc.createRestriction(t, "google.gmail", "send", "Blocked by test")

	reqID := fmt.Sprintf("req-audit-%s", randSuffix())
	sc.gatewayRequest(env, reqID, "google.gmail", "send")

	// Check audit log
	resp := sc.session.do("GET", "/api/audit?outcome=blocked", nil)
	body := mustStatus(t, resp, http.StatusOK)
	entries := arr(t, body, "entries")
	if len(entries) == 0 {
		t.Fatal("audit: expected at least one blocked entry")
	}
	entry := entries[0].(map[string]any)
	if entry["outcome"] != "blocked" {
		t.Errorf("audit entry outcome: expected blocked, got %v", entry["outcome"])
	}
	if entry["service"] != "google.gmail" {
		t.Errorf("audit entry service: expected google.gmail, got %v", entry["service"])
	}
}

func TestGateway_NoTaskID_Rejected(t *testing.T) {
	env := newTestEnv(t, newMockAdapter("google.gmail", "send"))
	sc := newScenario(t, env, "bot")

	result := sc.gatewayRequest(env, "req-no-task", "google.gmail", "send")
	if result["code"] != "TASK_REQUIRED" {
		t.Errorf("expected code=TASK_REQUIRED, got %v", result["code"])
	}
}

func TestGateway_Execute_WithTask(t *testing.T) {
	adapter := newMockAdapter("mock.echo", "echo").
		withResult("echo ok", map[string]any{"msg": "hello"})
	env := newTestEnv(t, adapter)

	sc := newScenario(t, env, "automation")
	// Pre-seed vault credential for this user (the gateway requires one)
	if err := env.Vault.Set(context.Background(), sc.session.UserID, "mock.echo", []byte("cred")); err != nil {
		t.Fatalf("vault seed: %v", err)
	}

	taskID := sc.createApprovedTask(t, env, "mock.echo", "echo", true)

	reqID := fmt.Sprintf("req-exec-%s", randSuffix())
	result := sc.gatewayRequestWithTask(env, reqID, "mock.echo", "echo", taskID)
	if result["status"] != "executed" {
		t.Errorf("execute: expected status=executed, got %v (full: %v)", result["status"], result)
	}
	if result["result"] == nil {
		t.Error("execute: result field missing")
	}
}

func TestGateway_Execute_AdapterError_ReturnsError(t *testing.T) {
	adapter := newMockAdapter("mock.fail", "run").
		withError(errors.New("downstream failure"))
	env := newTestEnv(t, adapter)

	sc := newScenario(t, env, "ci")
	if err := env.Vault.Set(context.Background(), sc.session.UserID, "mock.fail", []byte("cred")); err != nil {
		t.Fatalf("vault seed: %v", err)
	}

	taskID := sc.createApprovedTask(t, env, "mock.fail", "run", true)

	result := sc.gatewayRequestWithTask(env, fmt.Sprintf("req-fail-%s", randSuffix()), "mock.fail", "run", taskID)
	if result["status"] != "error" {
		t.Errorf("adapter error: expected status=error, got %v", result["status"])
	}
	if result["error"] == nil {
		t.Error("adapter error: error field missing")
	}
}

func TestTaskCreate_InactiveService_Rejected(t *testing.T) {
	adapter := newMockAdapter("mock.noauth", "go")
	env := newTestEnv(t, adapter)

	sc := newScenario(t, env, "runner")

	// No vault credential seeded — task creation should fail
	resp := env.do("POST", "/api/tasks", sc.AgentToken, map[string]any{
		"purpose": "test task",
		"authorized_actions": []map[string]any{{
			"service": "mock.noauth", "action": "go", "auto_execute": true,
		}},
	})
	body := mustStatus(t, resp, http.StatusBadRequest)
	if body["code"] != "INVALID_REQUEST" {
		t.Errorf("expected code=INVALID_REQUEST, got %v", body["code"])
	}
	msg, _ := body["error"].(string)
	strContains(t, msg, "not activated", "error message")
}

// ── Alias mismatch ───────────────────────────────────────────────────────────

// TestApproval_AliasPreserved verifies that approving a request for an aliased
// service correctly resolves the vault key including the alias.
func TestApproval_AliasPreserved(t *testing.T) {
	adapter := newMockAdapter("mock.echo", "echo").
		withResult("echo ok", map[string]any{"msg": "hello"})
	env := newTestEnv(t, adapter)
	sc := newScenario(t, env, "automation")

	// Activate under "work" alias → vault key = "mock.echo:work"
	if err := env.Vault.Set(context.Background(), sc.session.UserID, "mock.echo:work", []byte("cred")); err != nil {
		t.Fatalf("vault seed: %v", err)
	}

	// Task with auto_execute=false for "mock.echo:work" → per-request approval.
	taskID := sc.createApprovedTask(t, env, "mock.echo:work", "echo", false)
	reqID := fmt.Sprintf("req-alias-%s", randSuffix())
	result := sc.gatewayRequestWithTask(env, reqID, "mock.echo:work", "echo", taskID)
	if result["status"] != "pending" {
		t.Fatalf("expected status=pending, got %v (full: %v)", result["status"], result)
	}

	// Approve — marks as approved.
	resp := sc.session.do("POST", fmt.Sprintf("/api/approvals/%s/approve", reqID), nil)
	body := mustStatus(t, resp, http.StatusOK)
	if body["status"] != "approved" {
		t.Errorf("expected status=approved, got %v (error=%v)", body["status"], body["error"])
	}

	// Agent calls execute — should succeed using the aliased vault key.
	resp = env.do("POST", fmt.Sprintf("/api/gateway/request/%s/execute", reqID), sc.AgentToken, nil)
	body = mustStatus(t, resp, http.StatusOK)
	if body["status"] != "executed" {
		t.Errorf("expected status=executed, got %v (error=%v)", body["status"], body["error"])
	}
}

// TestGateway_AliasNotFound verifies that requesting a non-existent alias
// returns ALIAS_NOT_FOUND when the service has other aliases activated.
func TestGateway_AliasNotFound(t *testing.T) {
	adapter := newMockAdapter("mock.echo", "echo")
	env := newTestEnv(t, adapter)

	t.Run("non-default alias missing", func(t *testing.T) {
		sc := newScenario(t, env, "automation")
		// Activate default alias only.
		if err := env.Vault.Set(context.Background(), sc.session.UserID, "mock.echo", []byte("cred")); err != nil {
			t.Fatalf("vault seed: %v", err)
		}

		taskID := sc.createApprovedTask(t, env, "mock.echo", "echo", true)
		result := sc.gatewayRequestWithTask(env, fmt.Sprintf("req-noalias-%s", randSuffix()), "mock.echo:nonexistent", "echo", taskID)
		if result["code"] != "ALIAS_NOT_FOUND" {
			t.Errorf("expected code=ALIAS_NOT_FOUND, got %v", result["code"])
		}
		errMsg, _ := result["error"].(string)
		if !strings.Contains(errMsg, "nonexistent") {
			t.Errorf("error should mention the alias name, got: %s", errMsg)
		}
		if !strings.Contains(errMsg, "GET /api/skill/catalog") {
			t.Errorf("error should direct agent to the catalog endpoint, got: %s", errMsg)
		}
	})

	t.Run("default alias missing but other alias exists", func(t *testing.T) {
		sc := newScenario(t, env, "automation")
		// Activate both aliases so task creation passes validation.
		if err := env.Vault.Set(context.Background(), sc.session.UserID, "mock.echo:work", []byte("cred")); err != nil {
			t.Fatalf("vault seed: %v", err)
		}

		// createApprovedTask activates "mock.echo" (default). Create task while it exists.
		taskID := sc.createApprovedTask(t, env, "mock.echo", "echo", true)

		// Remove the default alias — only :work remains.
		if err := env.Vault.Delete(context.Background(), sc.session.UserID, "mock.echo"); err != nil {
			t.Fatalf("vault delete: %v", err)
		}

		result := sc.gatewayRequestWithTask(env, fmt.Sprintf("req-defmiss-%s", randSuffix()), "mock.echo", "echo", taskID)
		if result["code"] != "ALIAS_NOT_FOUND" {
			t.Errorf("expected code=ALIAS_NOT_FOUND, got %v (full: %v)", result["code"], result)
		}
		errMsg, _ := result["error"].(string)
		if !strings.Contains(errMsg, "default") {
			t.Errorf("error should mention the alias name, got: %s", errMsg)
		}
	})
}

// ── Standing task guards ──────────────────────────────────────────────────────

func TestStandingTask_RejectsExpiresInSeconds(t *testing.T) {
	env := newTestEnv(t, newMockAdapter("mock.echo", "echo"))
	sc := newScenario(t, env, "automation")
	sc.activateService(t, env, "mock.echo")

	resp := env.do("POST", "/api/tasks", sc.AgentToken, map[string]any{
		"purpose":            "bad combo",
		"lifetime":           "standing",
		"expires_in_seconds": 3600,
		"authorized_actions": []map[string]any{
			{"service": "mock.echo", "action": "echo", "auto_execute": true},
		},
	})
	body := mustStatus(t, resp, http.StatusBadRequest)
	if body["code"] != "INVALID_REQUEST" {
		t.Errorf("expected code=INVALID_REQUEST, got %v", body["code"])
	}
	msg, _ := body["error"].(string)
	strContains(t, msg, "expires_in_seconds cannot be set on a standing task", "error message")
}

func TestStandingTask_ResponseOmitsExpiry(t *testing.T) {
	env := newTestEnv(t, newMockAdapter("mock.echo", "echo"))
	sc := newScenario(t, env, "automation")

	taskID := sc.createApprovedStandingTask(t, env, "mock.echo", "echo", true)

	// GET as agent — expires_at and expires_in_seconds should be absent
	resp := env.do("GET", fmt.Sprintf("/api/tasks/%s", taskID), sc.AgentToken, nil)
	body := mustStatus(t, resp, http.StatusOK)
	if body["expires_at"] != nil {
		t.Errorf("standing task Get: expected expires_at to be absent, got %v", body["expires_at"])
	}
	if v, ok := body["expires_in_seconds"]; ok && v != nil && v != 0.0 {
		t.Errorf("standing task Get: expected expires_in_seconds absent/zero, got %v", v)
	}

	// LIST as user — check the task in the list
	resp = sc.session.do("GET", "/api/tasks", nil)
	listBody := mustStatus(t, resp, http.StatusOK)
	tasks := arr(t, listBody, "tasks")
	for _, raw := range tasks {
		task, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if task["id"] == taskID {
			if task["expires_at"] != nil {
				t.Errorf("standing task List: expected expires_at nil, got %v", task["expires_at"])
			}
			return
		}
	}
	t.Error("standing task not found in list response")
}

func TestStandingTask_Expand_Rejected(t *testing.T) {
	adapter := newMockAdapter("mock.echo", "echo", "other")
	env := newTestEnv(t, adapter)
	sc := newScenario(t, env, "automation")

	taskID := sc.createApprovedStandingTask(t, env, "mock.echo", "echo", true)

	// Try to expand the standing task — should fail
	resp := env.do("POST", fmt.Sprintf("/api/tasks/%s/expand", taskID), sc.AgentToken, map[string]any{
		"service": "mock.echo", "action": "other", "auto_execute": true, "reason": "need more",
	})
	body := mustStatus(t, resp, http.StatusConflict)
	if body["code"] != "INVALID_OPERATION" {
		t.Errorf("expected code=INVALID_OPERATION, got %v", body["code"])
	}
	msg, _ := body["error"].(string)
	strContains(t, msg, "standing tasks cannot be expanded", "error message")
}

func TestStandingTask_OutOfScope_MessageSuggestsNewTask(t *testing.T) {
	adapter := newMockAdapter("mock.echo", "echo", "other")
	env := newTestEnv(t, adapter)
	sc := newScenario(t, env, "automation")
	if err := env.Vault.Set(context.Background(), sc.session.UserID, "mock.echo", []byte("cred")); err != nil {
		t.Fatalf("vault seed: %v", err)
	}

	taskID := sc.createApprovedStandingTask(t, env, "mock.echo", "echo", true)

	// Request an action outside the standing task's scope
	result := sc.gatewayRequestWithTask(env, fmt.Sprintf("req-standing-oos-%s", randSuffix()), "mock.echo", "other", taskID)
	if result["status"] != "pending_scope_expansion" {
		t.Errorf("expected status=pending_scope_expansion, got %v", result["status"])
	}
	msg, _ := result["message"].(string)
	strContains(t, msg, "standing task", "gateway out-of-scope message for standing task")
	strContains(t, msg, "cannot be expanded", "gateway out-of-scope message for standing task")
}

// ── Scope expansion ───────────────────────────────────────────────────────────

func TestExpand_ReasonBecomesExpansionRationale(t *testing.T) {
	adapter := newMockAdapter("mock.echo", "echo", "other")
	env := newTestEnv(t, adapter)
	sc := newScenario(t, env, "automation")
	sc.activateService(t, env, "mock.echo")

	taskID := sc.createApprovedTask(t, env, "mock.echo", "echo", true)

	// Request expansion with a reason.
	resp := env.do("POST", fmt.Sprintf("/api/tasks/%s/expand", taskID), sc.AgentToken, map[string]any{
		"service": "mock.echo", "action": "other", "auto_execute": true,
		"reason": "Need to run other action for analysis",
	})
	mustStatus(t, resp, http.StatusAccepted)

	// Approve the expansion.
	resp = sc.session.do("POST", fmt.Sprintf("/api/tasks/%s/expand/approve", taskID), nil)
	mustStatus(t, resp, http.StatusOK)

	// Fetch the task and verify the expanded action has expected_use set to the reason.
	resp = env.do("GET", fmt.Sprintf("/api/tasks/%s", taskID), sc.AgentToken, nil)
	body := mustStatus(t, resp, http.StatusOK)
	actions := arr(t, body, "authorized_actions")

	found := false
	for _, raw := range actions {
		a, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if a["action"] == "other" {
			found = true
			er, _ := a["expansion_rationale"].(string)
			if er != "Need to run other action for analysis" {
				t.Errorf("expansion_rationale: got %q, want expansion reason", er)
			}
		}
	}
	if !found {
		t.Error("expanded action 'other' not found in authorized_actions")
	}
}

// ── Approvals ─────────────────────────────────────────────────────────────────

func TestApprovals_Deny(t *testing.T) {
	env := newTestEnv(t, newMockAdapter("mock.deny", "run"))
	sc := newScenario(t, env, "automation")

	taskID := sc.createApprovedTask(t, env, "mock.deny", "run", false)

	reqID := fmt.Sprintf("req-deny-%s", randSuffix())
	sc.gatewayRequestWithTask(env, reqID, "mock.deny", "run", taskID)

	// Deny it
	resp := sc.session.do("POST", fmt.Sprintf("/api/approvals/%s/deny", reqID), nil)
	body := mustStatus(t, resp, http.StatusOK)
	if body["status"] != "denied" {
		t.Errorf("deny: expected status=denied, got %v", body["status"])
	}

	// No longer in pending list
	resp = sc.session.do("GET", "/api/approvals", nil)
	apBody := mustStatus(t, resp, http.StatusOK)
	for _, e := range arr(t, apBody, "entries") {
		entry := e.(map[string]any)
		if entry["request_id"] == reqID {
			t.Errorf("denied request %q should not appear in pending list", reqID)
		}
	}

	// Audit entry should show denied
	resp = sc.session.do("GET", "/api/audit?outcome=denied", nil)
	auditBody := mustStatus(t, resp, http.StatusOK)
	if n, _ := auditBody["total"].(float64); n == 0 {
		t.Error("denied outcome: expected audit entry")
	}
}

func TestApprovals_Approve_WithMockAdapter(t *testing.T) {
	adapter := newMockAdapter("mock.ok", "run").
		withResult("done", nil)
	env := newTestEnv(t, adapter)

	sc := newScenario(t, env, "ci")

	taskID := sc.createApprovedTask(t, env, "mock.ok", "run", false)

	reqID := fmt.Sprintf("req-app-%s", randSuffix())
	sc.gatewayRequestWithTask(env, reqID, "mock.ok", "run", taskID)

	// Approve it — marks as approved but does not execute.
	resp := sc.session.do("POST", fmt.Sprintf("/api/approvals/%s/approve", reqID), nil)
	body := mustStatus(t, resp, http.StatusOK)
	if body["status"] != "approved" {
		t.Errorf("approve: expected status=approved, got %v", body["status"])
	}

	// Agent calls execute to claim the result.
	resp = env.do("POST", fmt.Sprintf("/api/gateway/request/%s/execute", reqID), sc.AgentToken, nil)
	body = mustStatus(t, resp, http.StatusOK)
	if body["status"] != "executed" {
		t.Errorf("execute: expected status=executed, got %v", body["status"])
	}
	if body["result"] == nil {
		t.Error("execute: result field missing")
	}
}

func TestApprovals_Approve_WrongUser_Forbidden(t *testing.T) {
	env := newTestEnv(t, newMockAdapter("mock.forbidden", "run"))
	sc1 := newScenario(t, env, "bot1")

	taskID := sc1.createApprovedTask(t, env, "mock.forbidden", "run", false)

	reqID := fmt.Sprintf("req-forbidden-%s", randSuffix())
	sc1.gatewayRequestWithTask(env, reqID, "mock.forbidden", "run", taskID)

	// Different user tries to approve
	s2 := newSession(t, env)
	resp := s2.do("POST", fmt.Sprintf("/api/approvals/%s/approve", reqID), nil)
	// Should be 404 (not found for this user) or 403 (forbidden)
	if resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusForbidden {
		t.Errorf("wrong user approve: expected 403/404, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestApprovals_UnknownID_Returns404(t *testing.T) {
	env := newTestEnv(t)
	s := newSession(t, env)

	resp := s.do("POST", "/api/approvals/nonexistent-id/approve", nil)
	mustStatus(t, resp, http.StatusNotFound)
}

// ── Audit ─────────────────────────────────────────────────────────────────────

func TestAudit_GetByID(t *testing.T) {
	env := newTestEnv(t)
	sc := newScenario(t, env, "automation")
	sc.createRestriction(t, "google.gmail", "send", "blocked for audit test")

	sc.gatewayRequest(env, fmt.Sprintf("req-audit-id-%s", randSuffix()), "google.gmail", "send")

	// Get list to find ID
	resp := sc.session.do("GET", "/api/audit", nil)
	listBody := mustStatus(t, resp, http.StatusOK)
	entries := arr(t, listBody, "entries")
	if len(entries) == 0 {
		t.Fatal("audit: no entries")
	}
	entry := entries[0].(map[string]any)
	id, ok := entry["id"].(string)
	if !ok || id == "" {
		t.Fatal("audit: entry id missing")
	}

	// Get single entry
	resp = sc.session.do("GET", fmt.Sprintf("/api/audit/%s", id), nil)
	single := mustStatus(t, resp, http.StatusOK)
	if single["id"] != id {
		t.Errorf("audit get by id: id mismatch")
	}
}

func TestAudit_FilterByService(t *testing.T) {
	env := newTestEnv(t)
	sc := newScenario(t, env, "automation")
	sc.createRestriction(t, "google.gmail", "send", "blocked for filter test")

	sc.gatewayRequest(env, fmt.Sprintf("req-filt-%s", randSuffix()), "google.gmail", "send")

	resp := sc.session.do("GET", "/api/audit?service=google.gmail", nil)
	body := mustStatus(t, resp, http.StatusOK)
	entries := arr(t, body, "entries")
	if len(entries) == 0 {
		t.Error("audit filter by service: expected entries")
	}
	for _, e := range entries {
		entry := e.(map[string]any)
		if entry["service"] != "google.gmail" {
			t.Errorf("audit filter: got service=%v, expected google.gmail", entry["service"])
		}
	}
}

func TestAudit_IsolatedByUser(t *testing.T) {
	env := newTestEnv(t)
	sc1 := newScenario(t, env, "bot")
	sc1.createRestriction(t, "google.gmail", "send", "blocked for isolation test")
	sc1.gatewayRequest(env, "req-iso-1", "google.gmail", "send")

	// Different user should see 0 entries
	s2 := newSession(t, env)
	resp := s2.do("GET", "/api/audit", nil)
	body := mustStatus(t, resp, http.StatusOK)
	if n, _ := body["total"].(float64); n != 0 {
		t.Errorf("audit isolation: user2 should see 0 entries, got %v", n)
	}
}

func TestAudit_AllOutcomesRecorded(t *testing.T) {
	// In one test, generate block and deny outcomes, then verify all appear.
	env := newTestEnv(t, newMockAdapter("mock.svc", "blocked-action", "approved-action"))
	sc := newScenario(t, env, "mixed")
	if err := env.Vault.Set(context.Background(), sc.session.UserID, "mock.svc", []byte("dummy")); err != nil {
		t.Fatalf("vault seed: %v", err)
	}

	sc.createRestriction(t, "mock.svc", "blocked-action", "blocked by test")

	// Block outcome
	sc.gatewayRequest(env, fmt.Sprintf("req-blk-%s", randSuffix()), "mock.svc", "blocked-action")

	// Task with auto_execute=false → per-request approval (pending)
	taskID := sc.createApprovedTask(t, env, "mock.svc", "approved-action", false)
	reqID := fmt.Sprintf("req-pend-%s", randSuffix())
	sc.gatewayRequestWithTask(env, reqID, "mock.svc", "approved-action", taskID)

	// Deny → denied
	sc.session.do("POST", fmt.Sprintf("/api/approvals/%s/deny", reqID), nil)

	resp := sc.session.do("GET", "/api/audit", nil)
	body := mustStatus(t, resp, http.StatusOK)
	entries := arr(t, body, "entries")

	outcomes := map[string]bool{}
	for _, e := range entries {
		entry := e.(map[string]any)
		if outcome, ok := entry["outcome"].(string); ok {
			outcomes[outcome] = true
		}
	}

	for _, want := range []string{"blocked", "denied"} {
		if !outcomes[want] {
			t.Errorf("audit: expected outcome=%q in entries, got outcomes=%v", want, outcomes)
		}
	}
	// "pending" was updated to "denied", so it should not appear
	if outcomes["pending"] {
		t.Error("audit: 'pending' should have been updated to 'denied' after deny action")
	}
}

// ── Services catalog ──────────────────────────────────────────────────────────

func TestServices_NoAdapters_EmptyList(t *testing.T) {
	env := newTestEnv(t) // no extra adapters
	s := newSession(t, env)

	resp := s.do("GET", "/api/services", nil)
	body := mustStatus(t, resp, http.StatusOK)
	services, ok := body["services"].([]any)
	if !ok {
		t.Fatal("services: expected array under 'services' key")
	}
	if len(services) != 0 {
		t.Errorf("services: expected 0 with no adapters, got %d", len(services))
	}
}

func TestServices_WithMockAdapter_Listed(t *testing.T) {
	adapter := newMockAdapter("mock.svc", "read", "write")
	env := newTestEnv(t, adapter)
	s := newSession(t, env)

	resp := s.do("GET", "/api/services", nil)
	body := mustStatus(t, resp, http.StatusOK)
	services, ok := body["services"].([]any)
	if !ok || len(services) != 1 {
		t.Fatalf("services: expected 1 service, got %v", body["services"])
	}
	svc := services[0].(map[string]any)
	if svc["id"] != "mock.svc" {
		t.Errorf("services: expected id=mock.svc, got %v", svc["id"])
	}
	if svc["status"] != "not_activated" {
		t.Errorf("services: expected status=not_activated, got %v", svc["status"])
	}
}

func TestServices_RequiresAuth(t *testing.T) {
	env := newTestEnv(t)

	resp := env.do("GET", "/api/services", "", nil)
	mustStatus(t, resp, http.StatusUnauthorized)
}

// ── Health ────────────────────────────────────────────────────────────────────

func TestHealth(t *testing.T) {
	env := newTestEnv(t)

	resp := env.do("GET", "/health", "", nil)
	body := mustStatus(t, resp, http.StatusOK)
	if body["status"] != "ok" {
		t.Errorf("health: expected status=ok, got %v", body["status"])
	}
}

func TestReady(t *testing.T) {
	env := newTestEnv(t)

	resp := env.do("GET", "/ready", "", nil)
	body := mustStatus(t, resp, http.StatusOK)
	if body["status"] != "ok" {
		t.Errorf("ready: expected status=ok, got %v", body["status"])
	}
}
