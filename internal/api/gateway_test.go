package api_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
)

// ── Roles ─────────────────────────────────────────────────────────────────────

func TestRoles_CRUD(t *testing.T) {
	env := newTestEnv(t)
	s := newSession(t, env)

	// Create
	resp := s.do("POST", "/api/roles", map[string]any{
		"name": "researcher", "description": "Read-only role",
	})
	body := mustStatus(t, resp, http.StatusCreated)
	id := str(t, body, "id")
	if id == "" {
		t.Fatal("create role: id empty")
	}
	if str(t, body, "name") != "researcher" {
		t.Errorf("create role: name mismatch")
	}

	// List — returns a bare array
	resp = s.do("GET", "/api/roles", nil)
	var roles []any
	decode(t, resp, &roles)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list roles: expected 200, got %d", resp.StatusCode)
	}
	if len(roles) != 1 {
		t.Errorf("list roles: expected 1, got %d", len(roles))
	}

	// Update
	resp = s.do("PUT", fmt.Sprintf("/api/roles/%s", id), map[string]any{
		"name": "analyst", "description": "Updated",
	})
	body = mustStatus(t, resp, http.StatusOK)
	if str(t, body, "name") != "analyst" {
		t.Errorf("update role: name not updated")
	}

	// Delete
	resp = s.do("DELETE", fmt.Sprintf("/api/roles/%s", id), nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("delete role: expected 204, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// List after delete
	resp = s.do("GET", "/api/roles", nil)
	var rolesAfter []any
	decode(t, resp, &rolesAfter)
	if len(rolesAfter) != 0 {
		t.Errorf("after delete: expected 0 roles, got %d", len(rolesAfter))
	}
}

func TestRoles_DuplicateName_Conflict(t *testing.T) {
	env := newTestEnv(t)
	s := newSession(t, env)

	s.do("POST", "/api/roles", map[string]any{"name": "ops"})
	resp := s.do("POST", "/api/roles", map[string]any{"name": "ops"})
	mustStatus(t, resp, http.StatusConflict)
}

func TestRoles_IsolatedByUser(t *testing.T) {
	env := newTestEnv(t)
	s1 := newSession(t, env)
	s2 := newSession(t, env)

	s1.do("POST", "/api/roles", map[string]any{"name": "admin"})

	resp := s2.do("GET", "/api/roles", nil)
	var roles []any
	decode(t, resp, &roles)
	if len(roles) != 0 {
		t.Errorf("user2 should see 0 roles, got %d", len(roles))
	}
}

// ── Policies ──────────────────────────────────────────────────────────────────

func TestPolicies_Create_ValidYAML(t *testing.T) {
	env := newTestEnv(t)
	s := newSession(t, env)

	// Role must exist before policy can reference it by name
	resp := s.do("POST", "/api/roles", map[string]any{"name": "automation"})
	mustStatus(t, resp, http.StatusCreated)

	yaml := blockPolicy("p-block-1", "automation", "google.gmail", "send")
	resp = s.do("POST", "/api/policies", map[string]any{"yaml": yaml})
	body := mustStatus(t, resp, http.StatusCreated)

	if str(t, body, "id") == "" {
		t.Error("create policy: id empty")
	}
	if str(t, body, "name") == "" {
		t.Error("create policy: name empty")
	}
}

func TestPolicies_Create_MissingYAML(t *testing.T) {
	env := newTestEnv(t)
	s := newSession(t, env)

	resp := s.do("POST", "/api/policies", map[string]any{"yaml": ""})
	mustStatus(t, resp, http.StatusBadRequest)
}

func TestPolicies_Create_InvalidYAML(t *testing.T) {
	env := newTestEnv(t)
	s := newSession(t, env)

	resp := s.do("POST", "/api/policies", map[string]any{"yaml": "not: valid: yaml: ::::"})
	// parser may succeed (YAML is permissive) but validation should catch missing fields
	// Just check we don't 500
	if resp.StatusCode == http.StatusInternalServerError {
		t.Errorf("invalid yaml should not cause 500, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestPolicies_List_ReturnsArray(t *testing.T) {
	env := newTestEnv(t)
	s := newSession(t, env)

	// List when empty
	resp := s.do("GET", "/api/policies", nil)
	var policies []any
	decode(t, resp, &policies)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list policies: expected 200, got %d", resp.StatusCode)
	}
	if policies == nil {
		t.Error("list policies: expected empty slice, got nil")
	}
}

func TestPolicies_Validate(t *testing.T) {
	env := newTestEnv(t)
	s := newSession(t, env)

	s.do("POST", "/api/roles", map[string]any{"name": "ops"})

	// Valid policy
	resp := s.do("POST", "/api/policies/validate", map[string]any{
		"yaml": blockPolicy("vp1", "ops", "google.gmail", "send"),
	})
	body := mustStatus(t, resp, http.StatusOK)
	if v, ok := body["valid"].(bool); !ok || !v {
		t.Errorf("validate: expected valid=true, got %v", body["valid"])
	}

	// Empty yaml → body-level error, still HTTP 200 (validate never 400s)
	resp = s.do("POST", "/api/policies/validate", map[string]any{"yaml": ""})
	// Note: /validate returns HTTP 400 for missing yaml field, not 200
	if resp.StatusCode != http.StatusBadRequest && resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Errorf("validate empty yaml: unexpected status %d", resp.StatusCode)
	} else {
		body = mustStatus(t, resp, resp.StatusCode)
		if v, _ := body["valid"].(bool); v {
			t.Error("validate empty yaml: expected valid=false")
		}
	}
}

func TestPolicies_Evaluate_NoRules_DefaultApprove(t *testing.T) {
	env := newTestEnv(t)
	s := newSession(t, env)

	resp := s.do("POST", "/api/policies/evaluate", map[string]any{
		"service": "google.gmail",
		"action":  "send",
	})
	body := mustStatus(t, resp, http.StatusOK)
	if str(t, body, "decision") != "approve" {
		t.Errorf("no rules: expected default decision=approve, got %q", str(t, body, "decision"))
	}
}

func TestPolicies_Evaluate_BlockRule(t *testing.T) {
	env := newTestEnv(t)
	s := newSession(t, env)

	s.do("POST", "/api/roles", map[string]any{"name": "bot"})
	s.do("POST", "/api/policies", map[string]any{
		"yaml": blockPolicy("p-eval-block", "bot", "google.gmail", "send"),
	})

	resp := s.do("POST", "/api/policies/evaluate", map[string]any{
		"service": "google.gmail",
		"action":  "send",
		"role":    "bot",
	})
	body := mustStatus(t, resp, http.StatusOK)
	if str(t, body, "decision") != "block" {
		t.Errorf("block rule: expected decision=block, got %q", str(t, body, "decision"))
	}
}

func TestPolicies_GetAndDelete(t *testing.T) {
	env := newTestEnv(t)
	s := newSession(t, env)

	s.do("POST", "/api/roles", map[string]any{"name": "ci"})
	resp := s.do("POST", "/api/policies", map[string]any{
		"yaml": blockPolicy("p-get-del", "ci", "google.gmail", "send"),
	})
	body := mustStatus(t, resp, http.StatusCreated)
	id := str(t, body, "id")

	// Get
	resp = s.do("GET", fmt.Sprintf("/api/policies/%s", id), nil)
	mustStatus(t, resp, http.StatusOK)

	// Delete
	resp = s.do("DELETE", fmt.Sprintf("/api/policies/%s", id), nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("delete policy: expected 204, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Get after delete → 404
	resp = s.do("GET", fmt.Sprintf("/api/policies/%s", id), nil)
	mustStatus(t, resp, http.StatusNotFound)
}

func TestPolicies_Update_ChangesRegistryDecision(t *testing.T) {
	env := newTestEnv(t)
	s := newSession(t, env)

	s.do("POST", "/api/roles", map[string]any{"name": "bot"})

	// Create a block policy with slug "update-test-block"
	resp := s.do("POST", "/api/policies", map[string]any{
		"yaml": blockPolicy("update-test-block", "bot", "google.gmail", "send"),
	})
	body := mustStatus(t, resp, http.StatusCreated)
	policyID := str(t, body, "id")

	// Confirm evaluate returns block
	resp = s.do("POST", "/api/policies/evaluate", map[string]any{
		"service": "google.gmail", "action": "send", "role": "bot",
	})
	body = mustStatus(t, resp, http.StatusOK)
	if str(t, body, "decision") != "block" {
		t.Fatalf("before update: expected block, got %q", str(t, body, "decision"))
	}

	// Update to an approve policy — note the YAML id changes to "update-test-approve"
	resp = s.do("PUT", fmt.Sprintf("/api/policies/%s", policyID), map[string]any{
		"yaml": approvePolicy("update-test-approve", "bot", "google.gmail", "send"),
	})
	mustStatus(t, resp, http.StatusOK)

	// Evaluate again — old block rules must be gone; should now return approve
	resp = s.do("POST", "/api/policies/evaluate", map[string]any{
		"service": "google.gmail", "action": "send", "role": "bot",
	})
	body = mustStatus(t, resp, http.StatusOK)
	if str(t, body, "decision") != "approve" {
		t.Errorf("after update: expected approve, got %q", str(t, body, "decision"))
	}
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

func TestGateway_Block(t *testing.T) {
	env := newTestEnv(t)
	sc := newScenario(t, env, "automation")

	sc.createPolicy(t, blockPolicy("p-gw-block", "automation", "google.gmail", "send"))

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
	sc.createPolicy(t, blockPolicy("p-audit-block", "automation", "google.gmail", "send"))

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

func TestGateway_DefaultApprove_NoPolicy(t *testing.T) {
	// Without any matching policy, the default decision is "approve".
	env := newTestEnv(t, newMockAdapter("google.gmail", "send"))
	sc := newScenario(t, env, "bot")
	if err := env.Vault.Set(context.Background(), sc.session.UserID, "google", []byte("dummy")); err != nil {
		t.Fatalf("vault seed: %v", err)
	}

	result := sc.gatewayRequest(env, "req-default-approve", "google.gmail", "send")
	if result["status"] != "pending" {
		t.Errorf("no policy: expected status=pending (default approve), got %v", result["status"])
	}
}

func TestGateway_ApprovePolicy_QueuesPending(t *testing.T) {
	env := newTestEnv(t, newMockAdapter("google.gmail", "send"))
	sc := newScenario(t, env, "automation")
	if err := env.Vault.Set(context.Background(), sc.session.UserID, "google", []byte("dummy")); err != nil {
		t.Fatalf("vault seed: %v", err)
	}
	sc.createPolicy(t, approvePolicy("p-gw-approve", "automation", "google.gmail", "send"))

	reqID := fmt.Sprintf("req-approve-%s", randSuffix())
	result := sc.gatewayRequest(env, reqID, "google.gmail", "send")
	if result["status"] != "pending" {
		t.Errorf("approve policy: expected status=pending, got %v", result["status"])
	}
	if result["request_id"] != reqID {
		t.Errorf("approve policy: request_id mismatch")
	}

	// Verify it shows up in approvals list
	resp := sc.session.do("GET", "/api/approvals", nil)
	apBody := mustStatus(t, resp, http.StatusOK)
	entries := arr(t, apBody, "entries")
	if len(entries) == 0 {
		t.Fatal("approvals: expected pending entry in list")
	}
}

func TestGateway_ExecutePolicy_WithMockAdapter(t *testing.T) {
	adapter := newMockAdapter("mock.echo", "echo").
		withResult("echo ok", map[string]any{"msg": "hello"})
	env := newTestEnv(t, adapter)

	sc := newScenario(t, env, "automation")
	sc.createPolicy(t, executePolicy("p-exec", "automation", "mock.echo", "echo"))

	// Pre-seed vault credential for this user (the gateway requires one)
	if err := env.Vault.Set(context.Background(), sc.session.UserID, "mock.echo", []byte("cred")); err != nil {
		t.Fatalf("vault seed: %v", err)
	}

	reqID := fmt.Sprintf("req-exec-%s", randSuffix())
	result := sc.gatewayRequest(env, reqID, "mock.echo", "echo")
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
	sc.createPolicy(t, executePolicy("p-fail", "ci", "mock.fail", "run"))

	if err := env.Vault.Set(context.Background(), sc.session.UserID, "mock.fail", []byte("cred")); err != nil {
		t.Fatalf("vault seed: %v", err)
	}

	result := sc.gatewayRequest(env, fmt.Sprintf("req-fail-%s", randSuffix()), "mock.fail", "run")
	if result["status"] != "error" {
		t.Errorf("adapter error: expected status=error, got %v", result["status"])
	}
	if result["error"] == nil {
		t.Error("adapter error: error field missing")
	}
}

func TestGateway_Execute_NoVaultCred_ReturnsPendingActivation(t *testing.T) {
	adapter := newMockAdapter("mock.noauth", "go")
	env := newTestEnv(t, adapter)

	sc := newScenario(t, env, "runner")
	sc.createPolicy(t, executePolicy("p-noauth", "runner", "mock.noauth", "go"))

	// No vault credential seeded — gateway should return pending_activation
	result := sc.gatewayRequest(env, fmt.Sprintf("req-na-%s", randSuffix()), "mock.noauth", "go")
	if result["status"] != "pending_activation" {
		t.Errorf("no vault cred: expected status=pending_activation, got %v", result["status"])
	}
}

// ── Approvals ─────────────────────────────────────────────────────────────────

func TestApprovals_Deny(t *testing.T) {
	env := newTestEnv(t, newMockAdapter("google.gmail", "send"))
	sc := newScenario(t, env, "automation")
	if err := env.Vault.Set(context.Background(), sc.session.UserID, "google", []byte("dummy")); err != nil {
		t.Fatalf("vault seed: %v", err)
	}
	sc.createPolicy(t, approvePolicy("p-deny", "automation", "google.gmail", "send"))

	reqID := fmt.Sprintf("req-deny-%s", randSuffix())
	sc.gatewayRequest(env, reqID, "google.gmail", "send")

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
	sc.createPolicy(t, approvePolicy("p-app-exec", "ci", "mock.ok", "run"))

	if err := env.Vault.Set(context.Background(), sc.session.UserID, "mock.ok", []byte("cred")); err != nil {
		t.Fatalf("vault seed: %v", err)
	}

	reqID := fmt.Sprintf("req-app-%s", randSuffix())
	sc.gatewayRequest(env, reqID, "mock.ok", "run")

	// Approve it — should execute successfully
	resp := sc.session.do("POST", fmt.Sprintf("/api/approvals/%s/approve", reqID), nil)
	body := mustStatus(t, resp, http.StatusOK)
	if body["status"] != "executed" {
		t.Errorf("approve+execute: expected status=executed, got %v", body["status"])
	}
	if body["result"] == nil {
		t.Error("approve+execute: result field missing")
	}
}

func TestApprovals_Approve_WrongUser_Forbidden(t *testing.T) {
	env := newTestEnv(t, newMockAdapter("google.gmail", "send"))
	sc1 := newScenario(t, env, "bot1")
	if err := env.Vault.Set(context.Background(), sc1.session.UserID, "google", []byte("dummy")); err != nil {
		t.Fatalf("vault seed: %v", err)
	}
	sc1.createPolicy(t, approvePolicy("p-forbidden", "bot1", "google.gmail", "send"))

	reqID := fmt.Sprintf("req-forbidden-%s", randSuffix())
	sc1.gatewayRequest(env, reqID, "google.gmail", "send")

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
	sc.createPolicy(t, blockPolicy("p-audit-id", "automation", "google.gmail", "send"))

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
	sc.createPolicy(t, blockPolicy("p-filt", "automation", "google.gmail", "send"))

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
	sc1.createPolicy(t, blockPolicy("p-iso", "bot", "google.gmail", "send"))
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
	// In one test, generate block, approve(→pending), and deny outcomes, then verify all appear.
	env := newTestEnv(t, newMockAdapter("mock.svc", "blocked-action", "approved-action"))
	sc := newScenario(t, env, "mixed")
	if err := env.Vault.Set(context.Background(), sc.session.UserID, "mock.svc", []byte("dummy")); err != nil {
		t.Fatalf("vault seed: %v", err)
	}

	sc.createPolicy(t, blockPolicy("p-mix-block", "mixed", "mock.svc", "blocked-action"))
	sc.createPolicy(t, approvePolicy("p-mix-approve", "mixed", "mock.svc", "approved-action"))

	// Block outcome
	sc.gatewayRequest(env, fmt.Sprintf("req-blk-%s", randSuffix()), "mock.svc", "blocked-action")

	// Approve → pending
	reqID := fmt.Sprintf("req-pend-%s", randSuffix())
	sc.gatewayRequest(env, reqID, "mock.svc", "approved-action")

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
