package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/clawvisor/clawvisor/pkg/config"
	"github.com/clawvisor/clawvisor/pkg/store"
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

	runtimeRules, err := env.Store.ListRuntimePolicyRules(context.Background(), s.UserID, store.RuntimePolicyRuleFilter{
		Kind: "service",
	})
	if err != nil {
		t.Fatalf("ListRuntimePolicyRules: %v", err)
	}
	if len(runtimeRules) != 1 {
		t.Fatalf("list runtime service rules: expected 1, got %d", len(runtimeRules))
	}
	rule := runtimeRules[0]
	if rule.Kind != "service" {
		t.Fatalf("runtime rule kind = %v, want service", rule.Kind)
	}
	if rule.Action != "deny" {
		t.Fatalf("runtime rule action = %v, want deny", rule.Action)
	}
	if rule.Service != "google.gmail" {
		t.Fatalf("runtime rule service = %v, want google.gmail", rule.Service)
	}
	if rule.ServiceAction != "send" {
		t.Fatalf("runtime rule service_action = %v, want send", rule.ServiceAction)
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
	if resp.StatusCode != http.StatusOK {
		t.Errorf("delete agent: expected 200, got %d", resp.StatusCode)
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

func TestGateway_NoTaskID_AutoBindsMatchingTask(t *testing.T) {
	adapter := newMockAdapter("mock.autobind", "run").
		withResult("ok", map[string]any{"status": "done"})
	env := newTestEnv(t, adapter)
	sc := newScenario(t, env, "bot")
	if err := env.Vault.Set(context.Background(), sc.session.UserID, "mock.autobind", []byte("cred")); err != nil {
		t.Fatalf("vault seed: %v", err)
	}

	taskID := sc.createApprovedTask(t, env, "mock.autobind", "run", true)
	result := sc.gatewayRequest(env, "req-no-task-auto", "mock.autobind", "run")
	if result["status"] != "executed" {
		t.Fatalf("expected executed status, got %v", result)
	}
	if _, ok := result["result"]; !ok {
		t.Fatalf("expected result payload, got %v", result)
	}

	entry, err := env.Store.GetAuditEntryByRequestID(context.Background(), "req-no-task-auto", sc.session.UserID)
	if err != nil {
		t.Fatalf("GetAuditEntryByRequestID: %v", err)
	}
	if entry.TaskID == nil || *entry.TaskID != taskID {
		t.Fatalf("expected auto-bound task_id %q, got %+v", taskID, entry.TaskID)
	}
}

func TestGateway_NoTaskID_AmbiguousTask(t *testing.T) {
	adapter := newMockAdapter("mock.ambiguous", "run")
	env := newTestEnv(t, adapter)
	sc := newScenario(t, env, "bot")
	if err := env.Vault.Set(context.Background(), sc.session.UserID, "mock.ambiguous", []byte("cred")); err != nil {
		t.Fatalf("vault seed: %v", err)
	}

	firstTaskID := sc.createApprovedTask(t, env, "mock.ambiguous", "run", true)
	resp := env.do("POST", "/api/tasks", sc.AgentToken, map[string]any{
		"purpose": "second ambiguous task",
		"authorized_actions": []map[string]any{{
			"service": "mock.ambiguous", "action": "run", "auto_execute": true,
		}},
	})
	body := mustStatus(t, resp, http.StatusCreated)
	secondTaskID := str(t, body, "task_id")
	if secondTaskID == firstTaskID {
		t.Fatalf("expected a distinct second task id, got %q", secondTaskID)
	}
	resp = sc.session.do("POST", fmt.Sprintf("/api/tasks/%s/approve", secondTaskID), nil)
	mustStatus(t, resp, http.StatusOK)

	result := sc.gatewayRequest(env, "req-no-task-ambiguous", "mock.ambiguous", "run")
	if result["code"] != "TASK_AMBIGUOUS" {
		t.Fatalf("expected TASK_AMBIGUOUS, got %v", result)
	}
	if result["classification"] != "ambiguous" {
		t.Fatalf("expected ambiguous classification, got %v", result)
	}
}

func TestGateway_NoTaskID_UncoveredRoutesToApproval(t *testing.T) {
	adapter := newMockAdapter("mock.uncovered", "run")
	env := newTestEnv(t, adapter)
	sc := newScenario(t, env, "bot")
	if err := env.Vault.Set(context.Background(), sc.session.UserID, "mock.uncovered", []byte("cred")); err != nil {
		t.Fatalf("vault seed: %v", err)
	}

	result := sc.gatewayRequest(env, "req-no-task-review", "mock.uncovered", "run")
	if result["status"] != "pending" {
		t.Fatalf("expected pending status, got %v", result)
	}
	if result["classification"] != "one_off" {
		t.Fatalf("expected one_off classification, got %v", result)
	}
}

func TestGateway_NoTaskID_ConcurrentApprovalRaceReturnsCanonicalPending(t *testing.T) {
	const goroutines = 6
	arrivedAtResolver := make(chan struct{}, goroutines)
	releaseResolver := make(chan struct{})
	llmSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&map[string]any{})
		arrivedAtResolver <- struct{}{}
		<-releaseResolver
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]any{"content": `{"kind":"one_off"}`}},
			},
		})
	}))
	defer llmSrv.Close()

	adapter := newMockAdapter("mock.no-task-race", "run")
	env := newTestEnvWithLLM(t, testGatewayResolverLLMConfig(llmSrv.URL), adapter, newMockAdapter("mock.no-task-race-other", "run"))
	sc := newScenario(t, env, "bot")
	if err := env.Vault.Set(context.Background(), sc.session.UserID, "mock.no-task-race", []byte("cred")); err != nil {
		t.Fatalf("vault seed: %v", err)
	}
	_ = sc.createApprovedTask(t, env, "mock.no-task-race-other", "run", true)
	reqID := fmt.Sprintf("req-no-task-race-%s", randSuffix())

	var wg sync.WaitGroup
	start := make(chan struct{})
	results := make([]map[string]any, goroutines)
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(idx int) {
			defer wg.Done()
			<-start
			results[idx] = sc.gatewayRequest(env, reqID, "mock.no-task-race", "run")
		}(i)
	}
	close(start)
	arrivals := 0
	select {
	case <-arrivedAtResolver:
		arrivals++
	case <-time.After(3 * time.Second):
		close(releaseResolver)
		t.Fatal("timed out waiting for first resolver request")
	}
	collectWindow := time.NewTimer(250 * time.Millisecond)
collectArrivals:
	for arrivals < goroutines {
		select {
		case <-arrivedAtResolver:
			arrivals++
		case <-collectWindow.C:
			break collectArrivals
		}
	}
	if !collectWindow.Stop() {
		select {
		case <-collectWindow.C:
		default:
		}
	}
	close(releaseResolver)
	wg.Wait()

	auditIDs := map[string]int{}
	dedupCount := 0
	for _, r := range results {
		if r == nil {
			t.Fatal("nil result")
		}
		if r["status"] != "pending" {
			t.Fatalf("expected status=pending, got %v (full: %+v)", r["status"], r)
		}
		auditIDs[str(t, r, "audit_id")]++
		if r["deduped"] == true {
			dedupCount++
		}
	}
	if len(auditIDs) != 1 {
		t.Fatalf("expected all racers to return canonical audit_id, got %d distinct: %v", len(auditIDs), auditIDs)
	}
	if dedupCount != goroutines-1 {
		t.Fatalf("expected %d racers to report deduped=true, got %d", goroutines-1, dedupCount)
	}

	resp := sc.session.do("GET", "/api/approvals", nil)
	body := mustStatus(t, resp, http.StatusOK)
	count := 0
	for _, e := range arr(t, body, "entries") {
		if e.(map[string]any)["request_id"] == reqID {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("expected exactly one pending approval for raced request_id, got %d", count)
	}
}

func TestGateway_NoTaskID_LLMResolvesAmbiguousTask(t *testing.T) {
	var llmResponse string
	llmSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&map[string]any{})
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]any{"content": llmResponse}},
			},
		})
	}))
	defer llmSrv.Close()

	adapter := newMockAdapter("mock.resolve-ambiguous", "run").
		withResult("ok", map[string]any{"status": "done"})
	env := newTestEnvWithLLM(t, testGatewayResolverLLMConfig(llmSrv.URL), adapter)
	sc := newScenario(t, env, "bot")
	if err := env.Vault.Set(context.Background(), sc.session.UserID, "mock.resolve-ambiguous", []byte("cred")); err != nil {
		t.Fatalf("vault seed: %v", err)
	}

	firstTaskID := createApprovedVerificationOffTask(t, env, sc, "first ambiguous task", "mock.resolve-ambiguous", "run")
	secondTaskID := createApprovedVerificationOffTask(t, env, sc, "second ambiguous task", "mock.resolve-ambiguous", "run")
	if firstTaskID == secondTaskID {
		t.Fatalf("expected distinct task ids, got %q", firstTaskID)
	}
	llmResponse = fmt.Sprintf(`{"kind":"belongs_to_existing_task","task_id":"%s"}`, secondTaskID)

	result := sc.gatewayRequest(env, "req-no-task-resolved-ambiguous", "mock.resolve-ambiguous", "run")
	if result["status"] != "executed" {
		t.Fatalf("expected executed status after resolver, got %v", result)
	}

	entry, err := env.Store.GetAuditEntryByRequestID(context.Background(), "req-no-task-resolved-ambiguous", sc.session.UserID)
	if err != nil {
		t.Fatalf("GetAuditEntryByRequestID: %v", err)
	}
	if entry.TaskID == nil || *entry.TaskID != secondTaskID {
		t.Fatalf("expected resolver-selected task_id %q, got %+v", secondTaskID, entry.TaskID)
	}
}

func TestGateway_NoTaskID_LLMResolvesNeedsNewTaskToOneOff(t *testing.T) {
	llmSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&map[string]any{})
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]any{"content": `{"kind":"one_off"}`}},
			},
		})
	}))
	defer llmSrv.Close()

	adapter := newMockAdapter("mock.resolve-review", "run")
	env := newTestEnvWithLLM(t, testGatewayResolverLLMConfig(llmSrv.URL), adapter)
	sc := newScenario(t, env, "bot")
	if err := env.Vault.Set(context.Background(), sc.session.UserID, "mock.resolve-review", []byte("cred")); err != nil {
		t.Fatalf("vault seed: %v", err)
	}
	if err := env.Vault.Set(context.Background(), sc.session.UserID, "mock.resolve-review:work", []byte("cred")); err != nil {
		t.Fatalf("aliased vault seed: %v", err)
	}

	_ = createApprovedVerificationOffTask(t, env, sc, "existing task on different alias", "mock.resolve-review:work", "run")
	result := sc.gatewayRequest(env, "req-no-task-llm-one-off", "mock.resolve-review", "run")
	if result["status"] != "pending" {
		t.Fatalf("expected pending status, got %v", result)
	}
	if result["classification"] != "one_off" {
		t.Fatalf("expected resolver to classify as one_off, got %v", result)
	}
}

func createApprovedVerificationOffTask(t *testing.T, env *testEnv, sc *scenario, purpose, service, action string) string {
	t.Helper()
	resp := env.do("POST", "/api/tasks", sc.AgentToken, map[string]any{
		"purpose": purpose,
		"authorized_actions": []map[string]any{{
			"service": service, "action": action, "auto_execute": true, "verification": "off",
		}},
	})
	body := mustStatus(t, resp, http.StatusCreated)
	taskID := str(t, body, "task_id")
	resp = sc.session.do("POST", fmt.Sprintf("/api/tasks/%s/approve", taskID), nil)
	mustStatus(t, resp, http.StatusOK)
	return taskID
}

func testGatewayResolverLLMConfig(endpoint string) config.LLMConfig {
	return config.LLMConfig{
		Provider:       "openai",
		Endpoint:       endpoint,
		Model:          "test-model",
		TimeoutSeconds: 5,
		Verification: config.VerificationConfig{
			LLMProviderConfig: config.LLMProviderConfig{
				Enabled:        true,
				Provider:       "openai",
				Endpoint:       endpoint,
				Model:          "test-model",
				TimeoutSeconds: 5,
			},
		},
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
	if body["code"] != "SERVICE_NOT_CONFIGURED" {
		t.Errorf("expected code=SERVICE_NOT_CONFIGURED, got %v", body["code"])
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
		if !strings.Contains(errMsg, "Available connections") {
			t.Errorf("error should list available connections, got: %s", errMsg)
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
		if !strings.Contains(errMsg, "No default account") {
			t.Errorf("error should mention the default alias, got: %s", errMsg)
		}
		if !strings.Contains(errMsg, "mock.echo:work") {
			t.Errorf("error should list available connections including :work, got: %s", errMsg)
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

// TestStandingTask_Expand_PreservesLifetime locks in the lift of the
// "standing tasks can't be expanded" rule. The expand path now
// accepts standing-lifetime tasks; the approve transition preserves
// the lifetime (the row stays at the far-future ExpiresAt sentinel
// rather than collapsing to now via the now + 0 math). Without this
// branch in taskApprovedExpiresAt, an approved standing-task
// expansion would immediately expire.
func TestStandingTask_Expand_PreservesLifetime(t *testing.T) {
	adapter := newMockAdapter("mock.echo", "echo", "other")
	env := newTestEnv(t, adapter)
	sc := newScenario(t, env, "automation")

	taskID := sc.createApprovedStandingTask(t, env, "mock.echo", "echo", true)

	resp := env.do("POST", fmt.Sprintf("/api/tasks/%s/expand", taskID), sc.AgentToken, map[string]any{
		"expected_tools": []map[string]any{
			{"tool_name": "mock.echo:other", "why": "need other action"},
		},
		"reason": "need more",
	})
	mustStatus(t, resp, http.StatusAccepted)

	resp = sc.session.do("POST", fmt.Sprintf("/api/tasks/%s/expand/approve", taskID), nil)
	body := mustStatus(t, resp, http.StatusOK)
	if body["status"] != "active" {
		t.Errorf("status after approve = %v, want active", body["status"])
	}
	// Standing tasks must NOT emit expires_at on the response, and
	// the row must remain at lifetime=standing with the same
	// no-expiry semantics it had before the expansion.
	if _, hasExpiry := body["expires_at"]; hasExpiry {
		t.Errorf("standing expand response leaked expires_at: %v", body["expires_at"])
	}

	resp = env.do("GET", fmt.Sprintf("/api/tasks/%s", taskID), sc.AgentToken, nil)
	body = mustStatus(t, resp, http.StatusOK)
	if body["lifetime"] != "standing" {
		t.Errorf("lifetime after expand = %v, want standing", body["lifetime"])
	}
	if body["status"] != "active" {
		t.Errorf("status after expand = %v, want active", body["status"])
	}
	if body["expires_at"] != nil {
		t.Errorf("standing task expires_at = %v, want nil (sentinel must be hidden on response)", body["expires_at"])
	}
}

func TestStandingTask_OutOfScope_MessageSuggestsExpand(t *testing.T) {
	adapter := newMockAdapter("mock.echo", "echo", "other")
	env := newTestEnv(t, adapter)
	sc := newScenario(t, env, "automation")
	if err := env.Vault.Set(context.Background(), sc.session.UserID, "mock.echo", []byte("cred")); err != nil {
		t.Fatalf("vault seed: %v", err)
	}

	taskID := sc.createApprovedStandingTask(t, env, "mock.echo", "echo", true)

	// Request an action outside the standing task's scope (with session_id to avoid MISSING_SESSION_ID error)
	result := sc.gatewayRequestWithTaskAndSession(env, fmt.Sprintf("req-standing-oos-%s", randSuffix()), "mock.echo", "other", taskID, "sess-standing-oos")
	if result["status"] != "pending_scope_expansion" {
		t.Errorf("expected status=pending_scope_expansion, got %v", result["status"])
	}
	msg, _ := result["message"].(string)
	// Lifting the standing-cannot-expand rule means the out-of-scope
	// message now steers the agent to expand instead of revoke+recreate.
	// The "lifetime preserved" wording is what we want the model to
	// see — it shouldn't think the standing-ness is at risk.
	strContains(t, msg, "standing task", "gateway out-of-scope message for standing task")
	strContains(t, msg, "/expand", "gateway out-of-scope message should suggest expand for standing tasks")
	strContains(t, msg, "lifetime will be preserved", "gateway out-of-scope message should reassure the model the standing lifetime survives")
}

func TestStandingTask_MissingSessionID_Error(t *testing.T) {
	adapter := newMockAdapter("mock.echo", "echo")
	env := newTestEnv(t, adapter)
	sc := newScenario(t, env, "automation")
	if err := env.Vault.Set(context.Background(), sc.session.UserID, "mock.echo", []byte("cred")); err != nil {
		t.Fatalf("vault seed: %v", err)
	}

	taskID := sc.createApprovedStandingTask(t, env, "mock.echo", "echo", true)

	// Gateway request with standing task but no session_id should return 400.
	resp := env.do("POST", "/api/gateway/request", sc.AgentToken, map[string]any{
		"service":    "mock.echo",
		"action":     "echo",
		"params":     map[string]any{"to": "bob@example.com"},
		"reason":     "test reason",
		"request_id": fmt.Sprintf("req-no-session-%s", randSuffix()),
		"task_id":    taskID,
	})
	body := mustStatus(t, resp, http.StatusBadRequest)
	if body["code"] != "MISSING_SESSION_ID" {
		t.Errorf("expected code=MISSING_SESSION_ID, got %v", body["code"])
	}
}

// ── Scope expansion ───────────────────────────────────────────────────────────

// TestExpand_EnvelopeMergedOnApprove verifies the v2 reshape: an
// envelope-shape expansion declaring "service:action" tool names
// (a) updates the envelope's expected_tools with the per-entry why,
// (b) derives a matching AuthorizedAction so the gateway path will
// accept the newly authorized scope, and
// (c) carries the per-entry why into the new TaskAction's ExpansionRationale
// for intent verification.
func TestExpand_EnvelopeMergedOnApprove(t *testing.T) {
	adapter := newMockAdapter("mock.echo", "echo", "other")
	env := newTestEnv(t, adapter)
	sc := newScenario(t, env, "automation")
	sc.activateService(t, env, "mock.echo")

	taskID := sc.createApprovedTask(t, env, "mock.echo", "echo", true)

	resp := env.do("POST", fmt.Sprintf("/api/tasks/%s/expand", taskID), sc.AgentToken, map[string]any{
		"expected_tools": []map[string]any{
			{"tool_name": "mock.echo:other", "why": "Need to run other action for analysis"},
		},
		"reason": "Discovered after fetching that we need to follow up via other action",
	})
	mustStatus(t, resp, http.StatusAccepted)

	resp = sc.session.do("POST", fmt.Sprintf("/api/tasks/%s/expand/approve", taskID), nil)
	mustStatus(t, resp, http.StatusOK)

	resp = env.do("GET", fmt.Sprintf("/api/tasks/%s", taskID), sc.AgentToken, nil)
	body := mustStatus(t, resp, http.StatusOK)
	if body["status"] != "active" {
		t.Errorf("status after approve = %v, want active", body["status"])
	}
	tools := arr(t, body, "expected_tools")
	found := false
	for _, raw := range tools {
		entry, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if entry["tool_name"] == "mock.echo:other" {
			found = true
			if entry["why"] != "Need to run other action for analysis" {
				t.Errorf("why = %v, want the agent-supplied why", entry["why"])
			}
		}
	}
	if !found {
		t.Errorf("expanded tool 'mock.echo:other' not found in expected_tools (have %v)", tools)
	}

	// AuthorizedAction derived + ExpansionRationale carried.
	actions := arr(t, body, "authorized_actions")
	derivedFound := false
	for _, raw := range actions {
		a, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if a["service"] == "mock.echo" && a["action"] == "other" {
			derivedFound = true
			if a["expansion_rationale"] != "Need to run other action for analysis" {
				t.Errorf("expansion_rationale = %v, want the per-entry why", a["expansion_rationale"])
			}
		}
	}
	if !derivedFound {
		t.Errorf("derived AuthorizedAction mock.echo:other not found (have %v) — gateway will reject the expanded scope", actions)
	}

	// End-to-end: the gateway path now accepts the newly authorized action.
	result := sc.gatewayRequestWithTask(env, fmt.Sprintf("req-postexpand-%s", randSuffix()), "mock.echo", "other", taskID)
	if status, _ := result["status"].(string); status == "out_of_scope" || status == "pending_scope_expansion" {
		t.Errorf("gateway status = %q after expansion approve; expected to be in scope (result=%+v)", status, result)
	}
}

// TestExpand_EmptyBodyRejected confirms an envelope-shape expansion
// request must declare at least one addition. An empty body would
// otherwise flip the task to pending_scope_expansion with nothing to
// approve, leaving the dashboard / chat surfaces with a no-op
// approval prompt.
func TestExpand_EmptyBodyRejected(t *testing.T) {
	adapter := newMockAdapter("mock.echo", "echo")
	env := newTestEnv(t, adapter)
	sc := newScenario(t, env, "automation")
	sc.activateService(t, env, "mock.echo")

	taskID := sc.createApprovedTask(t, env, "mock.echo", "echo", true)

	resp := env.do("POST", fmt.Sprintf("/api/tasks/%s/expand", taskID), sc.AgentToken, map[string]any{
		"reason": "forgot to include any actual additions",
	})
	body := mustStatus(t, resp, http.StatusBadRequest)
	if body["code"] != "INVALID_REQUEST" {
		t.Errorf("code = %v, want INVALID_REQUEST", body["code"])
	}
}

// TestExpand_DedupReplacesWhy exercises the load-bearing replace-by-name
// contract end-to-end: when the agent expands with a tool that already
// exists on the parent task, the merged envelope persisted on approve
// has exactly one entry per canonical tool name, with the new why.
func TestExpand_DedupReplacesWhy(t *testing.T) {
	adapter := newMockAdapter("mock.echo", "echo")
	env := newTestEnv(t, adapter)
	sc := newScenario(t, env, "automation")
	sc.activateService(t, env, "mock.echo")

	// Create a task carrying both an AuthorizedAction (so the v1 path
	// is happy) and an envelope-shape expected_tools entry that the
	// expansion will collide with.
	createBody := map[string]any{
		"purpose": "smoke — dedup replace",
		"authorized_actions": []map[string]any{
			{"service": "mock.echo", "action": "echo", "auto_execute": true},
		},
		"expected_tools": []map[string]any{
			{"tool_name": "Bash", "why": "run a single curl to list emails"},
		},
		"expires_in_seconds": 600,
	}
	resp := env.do("POST", "/api/tasks", sc.AgentToken, createBody)
	body := mustStatus(t, resp, http.StatusCreated)
	taskID := str(t, body, "task_id")
	resp = sc.session.do("POST", fmt.Sprintf("/api/tasks/%s/approve", taskID), nil)
	mustStatus(t, resp, http.StatusOK)

	// Expand with the same Bash entry but a new, broader why.
	resp = env.do("POST", fmt.Sprintf("/api/tasks/%s/expand", taskID), sc.AgentToken, map[string]any{
		"expected_tools": []map[string]any{
			{"tool_name": "Bash", "why": "list emails AND run a local processing script"},
		},
		"reason": "discovered we need local processing of the listed emails",
	})
	mustStatus(t, resp, http.StatusAccepted)

	resp = sc.session.do("POST", fmt.Sprintf("/api/tasks/%s/expand/approve", taskID), nil)
	mustStatus(t, resp, http.StatusOK)

	resp = env.do("GET", fmt.Sprintf("/api/tasks/%s", taskID), sc.AgentToken, nil)
	body = mustStatus(t, resp, http.StatusOK)
	tools := arr(t, body, "expected_tools")
	if len(tools) != 1 {
		t.Fatalf("expected_tools = %v, want exactly one entry after dedup-replace", tools)
	}
	entry, _ := tools[0].(map[string]any)
	if entry["tool_name"] != "Bash" {
		t.Errorf("tool_name = %v, want Bash", entry["tool_name"])
	}
	if entry["why"] != "list emails AND run a local processing script" {
		t.Errorf("why = %v, want the new broadened why (replace-by-name failed)", entry["why"])
	}
}

// TestExpand_CredentialsOnlyAccepted covers credentials-only expansion:
// the agent already has the tool/egress shape approved and just needs a
// newly-discovered credential. The handler must NOT require
// expected_tools or expected_egress in the body.
func TestExpand_CredentialsOnlyAccepted(t *testing.T) {
	adapter := newMockAdapter("mock.echo", "echo")
	env := newTestEnv(t, adapter)
	sc := newScenario(t, env, "automation")
	sc.activateService(t, env, "mock.echo")

	// Seed the vault item so the credential addition validates.
	if err := env.Vault.Set(context.Background(), sc.session.UserID, "mock.echo:account", []byte("cred")); err != nil {
		t.Fatalf("vault seed: %v", err)
	}

	taskID := sc.createApprovedTask(t, env, "mock.echo", "echo", true)

	resp := env.do("POST", fmt.Sprintf("/api/tasks/%s/expand", taskID), sc.AgentToken, map[string]any{
		"required_credentials": []map[string]any{
			{"vault_item_id": "mock.echo:account", "why": "Need credential for the already-approved echo action"},
		},
		"reason": "follow-up call needs an authenticated path",
	})
	mustStatus(t, resp, http.StatusAccepted)

	resp = sc.session.do("POST", fmt.Sprintf("/api/tasks/%s/expand/approve", taskID), nil)
	mustStatus(t, resp, http.StatusOK)
}

// TestExpand_DerivedActionRejectedForUnknownService confirms an
// expansion whose ExpectedTool names a service the user has no adapter
// for gets a 400 at request time — we will NOT land the pending state
// and waste user approval time only to discover the scope is unusable.
func TestExpand_DerivedActionRejectedForUnknownService(t *testing.T) {
	adapter := newMockAdapter("mock.echo", "echo")
	env := newTestEnv(t, adapter)
	sc := newScenario(t, env, "automation")
	sc.activateService(t, env, "mock.echo")

	taskID := sc.createApprovedTask(t, env, "mock.echo", "echo", true)

	resp := env.do("POST", fmt.Sprintf("/api/tasks/%s/expand", taskID), sc.AgentToken, map[string]any{
		"expected_tools": []map[string]any{
			{"tool_name": "mock.nonexistent:do_thing", "why": "would be unusable"},
		},
		"reason": "agent invented a service id",
	})
	body := mustStatus(t, resp, http.StatusBadRequest)
	if body["code"] != "INVALID_REQUEST" {
		t.Errorf("code = %v, want INVALID_REQUEST", body["code"])
	}
}

// TestExpand_DenyClearsPendingAndResolvesStatus exercises the atomic
// deny path end-to-end: after the user denies, the task returns to
// status=active, pending_expansion is cleared, and the canonical
// approval record is marked denied. Locks in the contract of
// ResolveTaskPendingExpansion so a future refactor can't split the
// clear-and-status update into a non-atomic pair again.
func TestExpand_DenyClearsPendingAndResolvesStatus(t *testing.T) {
	adapter := newMockAdapter("mock.echo", "echo", "other")
	env := newTestEnv(t, adapter)
	sc := newScenario(t, env, "automation")
	sc.activateService(t, env, "mock.echo")

	taskID := sc.createApprovedTask(t, env, "mock.echo", "echo", true)

	resp := env.do("POST", fmt.Sprintf("/api/tasks/%s/expand", taskID), sc.AgentToken, map[string]any{
		"expected_tools": []map[string]any{
			{"tool_name": "mock.echo:other", "why": "Need other action"},
		},
		"reason": "downstream summary step",
	})
	mustStatus(t, resp, http.StatusAccepted)

	// Mid-state check: status is pending, pending_expansion populated.
	resp = env.do("GET", fmt.Sprintf("/api/tasks/%s", taskID), sc.AgentToken, nil)
	body := mustStatus(t, resp, http.StatusOK)
	if body["status"] != "pending_scope_expansion" {
		t.Fatalf("status before deny = %v, want pending_scope_expansion", body["status"])
	}
	if body["pending_expansion"] == nil {
		t.Fatalf("pending_expansion missing before deny")
	}

	resp = sc.session.do("POST", fmt.Sprintf("/api/tasks/%s/expand/deny", taskID), nil)
	mustStatus(t, resp, http.StatusOK)

	resp = env.do("GET", fmt.Sprintf("/api/tasks/%s", taskID), sc.AgentToken, nil)
	body = mustStatus(t, resp, http.StatusOK)
	if body["status"] != "active" {
		t.Errorf("status after deny = %v, want active", body["status"])
	}
	if body["pending_expansion"] != nil {
		t.Errorf("pending_expansion = %v after deny; want cleared", body["pending_expansion"])
	}
	// AuthorizedActions must NOT include the proposed derived action.
	for _, raw := range arr(t, body, "authorized_actions") {
		a, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if a["service"] == "mock.echo" && a["action"] == "other" {
			t.Errorf("denied expansion's mock.echo:other appears in authorized_actions: %v", a)
		}
	}
}

// TestExpand_CrossAgentRejected asserts an agent under the same user
// cannot expand another agent's task. The owner gate (task.UserID !=
// agent.UserID) alone would let any sibling agent silently broaden a
// task it didn't create — the second-agent path is gated separately
// on task.AgentID. Without a dedicated test, a future refactor could
// re-loosen this without anyone noticing.
func TestExpand_CrossAgentRejected(t *testing.T) {
	adapter := newMockAdapter("mock.echo", "echo", "other")
	env := newTestEnv(t, adapter)
	sc := newScenario(t, env, "automation")
	sc.activateService(t, env, "mock.echo")

	taskID := sc.createApprovedTask(t, env, "mock.echo", "echo", true)

	// Create a second agent under the same user and obtain its token.
	resp := sc.session.do("POST", "/api/agents", map[string]any{
		"name": "test-agent-2",
	})
	body := mustStatus(t, resp, http.StatusCreated)
	secondAgentToken := str(t, body, "token")

	resp = env.do("POST", fmt.Sprintf("/api/tasks/%s/expand", taskID), secondAgentToken, map[string]any{
		"expected_tools": []map[string]any{
			{"tool_name": "mock.echo:other", "why": "sibling agent shouldn't be able to expand"},
		},
		"reason": "cross-agent expansion attempt",
	})
	body = mustStatus(t, resp, http.StatusForbidden)
	if body["code"] != "FORBIDDEN" {
		t.Errorf("code = %v, want FORBIDDEN", body["code"])
	}
}

// TestExpand_ConcurrentApproveAndDenyRace asserts the CAS guards on
// UpdateTaskEnvelopeFrom (approve) and ResolveTaskPendingExpansion
// (deny) jointly admit exactly one writer when both fire against the
// same pending row. The single existing deny test covers the typed
// enum's correctness; this test fires them as goroutines so a future
// refactor that splits clear-and-status into a non-atomic pair (or
// loses the pending_scope_expansion CAS) regresses loudly.
func TestExpand_ConcurrentApproveAndDenyRace(t *testing.T) {
	adapter := newMockAdapter("mock.echo", "echo", "other")
	env := newTestEnv(t, adapter)
	sc := newScenario(t, env, "automation")
	sc.activateService(t, env, "mock.echo")

	taskID := sc.createApprovedTask(t, env, "mock.echo", "echo", true)

	resp := env.do("POST", fmt.Sprintf("/api/tasks/%s/expand", taskID), sc.AgentToken, map[string]any{
		"expected_tools": []map[string]any{
			{"tool_name": "mock.echo:other", "why": "Need other action"},
		},
		"reason": "race-test",
	})
	mustStatus(t, resp, http.StatusAccepted)

	var wg sync.WaitGroup
	wg.Add(2)
	statusCh := make(chan int, 2)
	go func() {
		defer wg.Done()
		r := sc.session.do("POST", fmt.Sprintf("/api/tasks/%s/expand/approve", taskID), nil)
		defer r.Body.Close()
		statusCh <- r.StatusCode
	}()
	go func() {
		defer wg.Done()
		r := sc.session.do("POST", fmt.Sprintf("/api/tasks/%s/expand/deny", taskID), nil)
		defer r.Body.Close()
		statusCh <- r.StatusCode
	}()
	wg.Wait()
	close(statusCh)

	var oks, conflicts int
	for s := range statusCh {
		switch s {
		case http.StatusOK:
			oks++
		case http.StatusConflict:
			conflicts++
		default:
			t.Errorf("unexpected status %d", s)
		}
	}
	if oks != 1 || conflicts != 1 {
		t.Fatalf("race outcome: oks=%d conflicts=%d, want exactly 1 each", oks, conflicts)
	}

	// Final task state must match whichever side won — either active
	// with the merged scope OR active without it (parent scope only).
	// Both pending_expansion and status must be coherent: a denied
	// row returns to active with no pending JSON; an approved row is
	// active with no pending JSON too. The crucial invariant is that
	// pending_expansion is cleared either way.
	resp = env.do("GET", fmt.Sprintf("/api/tasks/%s", taskID), sc.AgentToken, nil)
	body := mustStatus(t, resp, http.StatusOK)
	if body["status"] != "active" {
		t.Errorf("status after race = %v, want active", body["status"])
	}
	if body["pending_expansion"] != nil {
		t.Errorf("pending_expansion = %v after race; want cleared regardless of winner", body["pending_expansion"])
	}
}

// TestExpand_StaleApproveRejectedAfterDenyAndReExpand exercises the
// pending-snapshot CAS guard on UpdateTaskEnvelopeFrom. Sequence:
// (1) agent expands → pending #A;
// (2) approve handler reads task with pending #A;
// (3) before the approve writes, a different caller denies (clearing
//     pending);
// (4) agent expands again → pending #B;
// (5) the original approve from (2) finally writes — its CAS must
//     LOSE because pending_expansion_json no longer matches snapshot
//     #A, even though status is again 'pending_scope_expansion'.
// Without the snapshot guard, the stale approve would grant scope
// #A that the user already denied. The store-layer test goes
// directly through UpdateTaskEnvelopeFrom so we exercise the SQL
// guard (the handler always sets ExpectedPendingJSON, so the only
// way to assert behavior at the bytes level is from the store).
func TestExpand_StaleApproveRejectedAfterDenyAndReExpand(t *testing.T) {
	adapter := newMockAdapter("mock.echo", "echo", "other")
	env := newTestEnv(t, adapter)
	sc := newScenario(t, env, "automation")
	sc.activateService(t, env, "mock.echo")

	taskID := sc.createApprovedTask(t, env, "mock.echo", "echo", true)

	// Step 1: pending #A
	resp := env.do("POST", fmt.Sprintf("/api/tasks/%s/expand", taskID), sc.AgentToken, map[string]any{
		"expected_tools": []map[string]any{
			{"tool_name": "mock.echo:other", "why": "Need A"},
		},
		"reason": "A",
	})
	mustStatus(t, resp, http.StatusAccepted)

	// Step 2: snapshot the task with pending #A
	resp = env.do("GET", fmt.Sprintf("/api/tasks/%s", taskID), sc.AgentToken, nil)
	mustStatus(t, resp, http.StatusOK)
	snapshotA, err := env.Store.GetTask(context.Background(), taskID)
	if err != nil {
		t.Fatalf("snapshot A: %v", err)
	}
	if snapshotA.PendingExpansion == nil {
		t.Fatalf("snapshot A has no PendingExpansion")
	}
	pendingA, err := json.Marshal(snapshotA.PendingExpansion)
	if err != nil {
		t.Fatalf("marshal pending A: %v", err)
	}

	// Step 3: deny — clears pending and restores status='active'
	resp = sc.session.do("POST", fmt.Sprintf("/api/tasks/%s/expand/deny", taskID), nil)
	mustStatus(t, resp, http.StatusOK)

	// Step 4: pending #B
	resp = env.do("POST", fmt.Sprintf("/api/tasks/%s/expand", taskID), sc.AgentToken, map[string]any{
		"expected_tools": []map[string]any{
			{"tool_name": "mock.echo:other", "why": "Need B (different reason)"},
		},
		"reason": "B",
	})
	mustStatus(t, resp, http.StatusAccepted)

	// Step 5: build envUpdate from snapshot A and attempt the stale
	// write with the snapshot guard. CAS must LOSE because the
	// stored pending_expansion_json now matches B's bytes, not A's.
	envUpdate := store.TaskEnvelopeUpdate{
		AuthorizedActions:   snapshotA.AuthorizedActions,
		ExpectedTools:       snapshotA.ExpectedTools,
		ExpectedEgress:      snapshotA.ExpectedEgress,
		RequiredCredentials: snapshotA.RequiredCredentials,
		ExpectedPendingJSON: pendingA,
	}
	won, err := env.Store.UpdateTaskEnvelopeFrom(
		context.Background(),
		taskID,
		"pending_scope_expansion",
		envUpdate,
		time.Now().UTC().Add(time.Hour),
	)
	if err != nil {
		t.Fatalf("UpdateTaskEnvelopeFrom: %v", err)
	}
	if won {
		t.Fatalf("CAS won with stale snapshot — pending-snapshot guard regressed; the deny+re-expand sequence must reject the stale approve")
	}

	// Sanity: a write with the CURRENT (B) snapshot still succeeds.
	snapshotB, err := env.Store.GetTask(context.Background(), taskID)
	if err != nil {
		t.Fatalf("snapshot B: %v", err)
	}
	pendingB, err := json.Marshal(snapshotB.PendingExpansion)
	if err != nil {
		t.Fatalf("marshal pending B: %v", err)
	}
	envUpdate.ExpectedPendingJSON = pendingB
	won, err = env.Store.UpdateTaskEnvelopeFrom(
		context.Background(),
		taskID,
		"pending_scope_expansion",
		envUpdate,
		time.Now().UTC().Add(time.Hour),
	)
	if err != nil {
		t.Fatalf("UpdateTaskEnvelopeFrom with current snapshot: %v", err)
	}
	if !won {
		t.Fatalf("CAS lost with the current pending snapshot; the guard should only reject stale writes")
	}
}

// TestExpand_DerivedActionRejectedForUnsupportedAction confirms that a
// service:action where the service exists but the adapter does not
// support that action is rejected up front.
func TestExpand_DerivedActionRejectedForUnsupportedAction(t *testing.T) {
	adapter := newMockAdapter("mock.echo", "echo")
	env := newTestEnv(t, adapter)
	sc := newScenario(t, env, "automation")
	sc.activateService(t, env, "mock.echo")

	taskID := sc.createApprovedTask(t, env, "mock.echo", "echo", true)

	resp := env.do("POST", fmt.Sprintf("/api/tasks/%s/expand", taskID), sc.AgentToken, map[string]any{
		"expected_tools": []map[string]any{
			{"tool_name": "mock.echo:fictional_action", "why": "agent imagined an action"},
		},
		"reason": "test",
	})
	body := mustStatus(t, resp, http.StatusBadRequest)
	if body["code"] != "INVALID_REQUEST" {
		t.Errorf("code = %v, want INVALID_REQUEST", body["code"])
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

// TestApprovals_DoubleApprove_Returns409 is the regression guard against
// a lost-CAS approve race surfacing as 500 INTERNAL_ERROR. The second
// approve call has nothing legitimate to do — the row is already
// resolved — but it's a normal race outcome (two dashboard tabs, etc.),
// not an internal failure. Must be 409 ALREADY_RESOLVED.
func TestApprovals_DoubleApprove_Returns409(t *testing.T) {
	adapter := newMockAdapter("mock.dbl", "run").withResult("done", nil)
	env := newTestEnv(t, adapter)
	sc := newScenario(t, env, "dbl")
	taskID := sc.createApprovedTask(t, env, "mock.dbl", "run", false)
	reqID := fmt.Sprintf("req-dbl-%s", randSuffix())
	sc.gatewayRequestWithTask(env, reqID, "mock.dbl", "run", taskID)

	// First approve → 200.
	resp := sc.session.do("POST", fmt.Sprintf("/api/approvals/%s/approve", reqID), nil)
	mustStatus(t, resp, http.StatusOK)

	// Second approve → 409 ALREADY_RESOLVED, not 500 INTERNAL_ERROR.
	resp = sc.session.do("POST", fmt.Sprintf("/api/approvals/%s/approve", reqID), nil)
	body := mustStatus(t, resp, http.StatusConflict)
	if code, _ := body["code"].(string); code != "ALREADY_RESOLVED" {
		t.Fatalf("expected code=ALREADY_RESOLVED, got %q (body=%v)", code, body)
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

// ── Audit integrity ───────────────────────────────────────────────────────────
//
// Every gateway request MUST produce an audit entry linked to its task, with
// the correct final outcome. These tests guard against regressions where
// requests silently disappear from the audit log.

func TestAudit_ApproveExecute_HasCorrectOutcomeAndTaskID(t *testing.T) {
	// Full approve+execute flow: the audit entry must end with outcome=executed
	// and be linked to the originating task.
	adapter := newMockAdapter("mock.audit", "run").withResult("ok", nil)
	env := newTestEnv(t, adapter)
	sc := newScenario(t, env, "audit-exec")

	taskID := sc.createApprovedTask(t, env, "mock.audit", "run", false)
	reqID := fmt.Sprintf("audit-exec-%s", randSuffix())
	sc.gatewayRequestWithTask(env, reqID, "mock.audit", "run", taskID)

	// Approve.
	resp := sc.session.do("POST", fmt.Sprintf("/api/approvals/%s/approve", reqID), nil)
	mustStatus(t, resp, http.StatusOK)

	// Verify audit shows "approved" before execute.
	resp = sc.session.do("GET", fmt.Sprintf("/api/audit?task_id=%s", taskID), nil)
	body := mustStatus(t, resp, http.StatusOK)
	entries := arr(t, body, "entries")
	if len(entries) == 0 {
		t.Fatal("audit: no entries found for task after approve")
	}
	entry := entries[0].(map[string]any)
	if entry["outcome"] != "approved" {
		t.Errorf("audit after approve: expected outcome=approved, got %v", entry["outcome"])
	}
	if entry["task_id"] != taskID {
		t.Errorf("audit after approve: expected task_id=%s, got %v", taskID, entry["task_id"])
	}

	// Execute.
	resp = env.do("POST", fmt.Sprintf("/api/gateway/request/%s/execute", reqID), sc.AgentToken, nil)
	mustStatus(t, resp, http.StatusOK)

	// Verify audit updated to "executed" and still linked to the task.
	resp = sc.session.do("GET", fmt.Sprintf("/api/audit?task_id=%s", taskID), nil)
	body = mustStatus(t, resp, http.StatusOK)
	entries = arr(t, body, "entries")
	if len(entries) == 0 {
		t.Fatal("audit: no entries found for task after execute")
	}
	entry = entries[0].(map[string]any)
	if entry["outcome"] != "executed" {
		t.Errorf("audit after execute: expected outcome=executed, got %v", entry["outcome"])
	}
	if entry["task_id"] != taskID {
		t.Errorf("audit after execute: expected task_id=%s, got %v", taskID, entry["task_id"])
	}
	if entry["request_id"] != reqID {
		t.Errorf("audit after execute: expected request_id=%s, got %v", reqID, entry["request_id"])
	}
	if entry["service"] != "mock.audit" {
		t.Errorf("audit after execute: expected service=mock.audit, got %v", entry["service"])
	}
}

func TestAudit_ApproveExecute_AdapterError_RecordedInAudit(t *testing.T) {
	// When the adapter fails during execute, the audit must show outcome=error
	// with the error message — not silently disappear.
	adapter := newMockAdapter("mock.audit-err", "run").
		withError(fmt.Errorf("adapter exploded"))
	env := newTestEnv(t, adapter)
	sc := newScenario(t, env, "audit-err")

	taskID := sc.createApprovedTask(t, env, "mock.audit-err", "run", false)
	reqID := fmt.Sprintf("audit-err-%s", randSuffix())
	sc.gatewayRequestWithTask(env, reqID, "mock.audit-err", "run", taskID)

	// Approve + execute.
	resp := sc.session.do("POST", fmt.Sprintf("/api/approvals/%s/approve", reqID), nil)
	mustStatus(t, resp, http.StatusOK)
	resp = env.do("POST", fmt.Sprintf("/api/gateway/request/%s/execute", reqID), sc.AgentToken, nil)
	execBody := mustStatus(t, resp, http.StatusOK)
	if execBody["status"] != "error" {
		t.Fatalf("execute: expected status=error, got %v", execBody["status"])
	}

	// Verify audit records the error.
	resp = sc.session.do("GET", fmt.Sprintf("/api/audit?task_id=%s", taskID), nil)
	body := mustStatus(t, resp, http.StatusOK)
	entries := arr(t, body, "entries")
	if len(entries) == 0 {
		t.Fatal("audit: no entries found for task after failed execute")
	}
	entry := entries[0].(map[string]any)
	if entry["outcome"] != "error" {
		t.Errorf("audit after error: expected outcome=error, got %v", entry["outcome"])
	}
	if entry["task_id"] != taskID {
		t.Errorf("audit after error: expected task_id=%s, got %v", taskID, entry["task_id"])
	}
	errMsg, _ := entry["error_msg"].(string)
	if errMsg == "" {
		t.Error("audit after error: error_msg should be populated")
	}
}

func TestAudit_AutoExecute_HasTaskID(t *testing.T) {
	// Auto-executed requests must also be linked to the task in the audit log.
	adapter := newMockAdapter("mock.audit-auto", "run").withResult("ok", nil)
	env := newTestEnv(t, adapter)
	sc := newScenario(t, env, "audit-auto")
	if err := env.Vault.Set(context.Background(), sc.session.UserID, "mock.audit-auto", []byte("cred")); err != nil {
		t.Fatalf("vault seed: %v", err)
	}

	taskID := sc.createApprovedTask(t, env, "mock.audit-auto", "run", true)
	reqID := fmt.Sprintf("audit-auto-%s", randSuffix())
	result := sc.gatewayRequestWithTask(env, reqID, "mock.audit-auto", "run", taskID)
	if result["status"] != "executed" {
		t.Fatalf("auto-execute: expected status=executed, got %v", result["status"])
	}

	resp := sc.session.do("GET", fmt.Sprintf("/api/audit?task_id=%s", taskID), nil)
	body := mustStatus(t, resp, http.StatusOK)
	entries := arr(t, body, "entries")
	if len(entries) == 0 {
		t.Fatal("audit: no entries found for auto-executed request")
	}
	entry := entries[0].(map[string]any)
	if entry["outcome"] != "executed" {
		t.Errorf("audit auto-execute: expected outcome=executed, got %v", entry["outcome"])
	}
	if entry["task_id"] != taskID {
		t.Errorf("audit auto-execute: expected task_id=%s, got %v", taskID, entry["task_id"])
	}
	if entry["request_id"] != reqID {
		t.Errorf("audit auto-execute: expected request_id=%s, got %v", reqID, entry["request_id"])
	}
}

func TestAudit_Deny_HasTaskID(t *testing.T) {
	// Denied requests must remain in the audit log linked to the task.
	env := newTestEnv(t, newMockAdapter("mock.audit-deny", "run"))
	sc := newScenario(t, env, "audit-deny")

	taskID := sc.createApprovedTask(t, env, "mock.audit-deny", "run", false)
	reqID := fmt.Sprintf("audit-deny-%s", randSuffix())
	sc.gatewayRequestWithTask(env, reqID, "mock.audit-deny", "run", taskID)

	// Deny.
	resp := sc.session.do("POST", fmt.Sprintf("/api/approvals/%s/deny", reqID), nil)
	mustStatus(t, resp, http.StatusOK)

	resp = sc.session.do("GET", fmt.Sprintf("/api/audit?task_id=%s", taskID), nil)
	body := mustStatus(t, resp, http.StatusOK)
	entries := arr(t, body, "entries")
	if len(entries) == 0 {
		t.Fatal("audit: no entries found for denied request")
	}
	entry := entries[0].(map[string]any)
	if entry["outcome"] != "denied" {
		t.Errorf("audit deny: expected outcome=denied, got %v", entry["outcome"])
	}
	if entry["task_id"] != taskID {
		t.Errorf("audit deny: expected task_id=%s, got %v", taskID, entry["task_id"])
	}
}

func TestAudit_Execute_NotApproved_NoAuditCorruption(t *testing.T) {
	// Calling /execute on a pending (not yet approved) request must fail
	// without corrupting the audit entry.
	env := newTestEnv(t, newMockAdapter("mock.audit-noexec", "run"))
	sc := newScenario(t, env, "audit-noexec")

	taskID := sc.createApprovedTask(t, env, "mock.audit-noexec", "run", false)
	reqID := fmt.Sprintf("audit-noexec-%s", randSuffix())
	sc.gatewayRequestWithTask(env, reqID, "mock.audit-noexec", "run", taskID)

	// Try to execute without approving first — returns pending, not executed.
	resp := env.do("POST", fmt.Sprintf("/api/gateway/request/%s/execute", reqID), sc.AgentToken, nil)
	execBody := mustStatus(t, resp, http.StatusAccepted)
	if execBody["status"] != "pending" {
		t.Errorf("execute before approve: expected status=pending, got %v", execBody["status"])
	}

	// Audit entry should still be "pending" and intact.
	resp = sc.session.do("GET", fmt.Sprintf("/api/audit?task_id=%s", taskID), nil)
	body := mustStatus(t, resp, http.StatusOK)
	entries := arr(t, body, "entries")
	if len(entries) == 0 {
		t.Fatal("audit: entry disappeared after rejected execute attempt")
	}
	entry := entries[0].(map[string]any)
	if entry["outcome"] != "pending" {
		t.Errorf("audit: expected outcome=pending (unchanged), got %v", entry["outcome"])
	}
	if entry["task_id"] != taskID {
		t.Errorf("audit: task_id should be intact, got %v", entry["task_id"])
	}
}

func TestAudit_StatusEndpoint_ReflectsApproval(t *testing.T) {
	// The status endpoint must reflect the "approved" state after approval
	// and "executed" after the agent calls execute.
	adapter := newMockAdapter("mock.audit-status", "run").withResult("ok", nil)
	env := newTestEnv(t, adapter)
	sc := newScenario(t, env, "audit-status")

	taskID := sc.createApprovedTask(t, env, "mock.audit-status", "run", false)
	reqID := fmt.Sprintf("audit-status-%s", randSuffix())
	sc.gatewayRequestWithTask(env, reqID, "mock.audit-status", "run", taskID)

	// Status before approval: pending.
	resp := env.do("GET", fmt.Sprintf("/api/gateway/request/%s", reqID), sc.AgentToken, nil)
	body := mustStatus(t, resp, http.StatusOK)
	if body["status"] != "pending" {
		t.Errorf("status before approve: expected pending, got %v", body["status"])
	}

	// Approve.
	resp = sc.session.do("POST", fmt.Sprintf("/api/approvals/%s/approve", reqID), nil)
	mustStatus(t, resp, http.StatusOK)

	// Status after approval: approved.
	resp = env.do("GET", fmt.Sprintf("/api/gateway/request/%s", reqID), sc.AgentToken, nil)
	body = mustStatus(t, resp, http.StatusOK)
	if body["status"] != "approved" {
		t.Errorf("status after approve: expected approved, got %v", body["status"])
	}

	// Execute.
	resp = env.do("POST", fmt.Sprintf("/api/gateway/request/%s/execute", reqID), sc.AgentToken, nil)
	mustStatus(t, resp, http.StatusOK)

	// Status after execute: executed.
	resp = env.do("GET", fmt.Sprintf("/api/gateway/request/%s", reqID), sc.AgentToken, nil)
	body = mustStatus(t, resp, http.StatusOK)
	if body["status"] != "executed" {
		t.Errorf("status after execute: expected executed, got %v", body["status"])
	}
}

func TestGateway_WaitTrue_DeniedRequest_ReturnsDenied(t *testing.T) {
	// Bug regression: when a request is denied while an agent is long-polling
	// with wait=true, the deny deletes the pending_approvals row. The long-poll
	// must detect this via the audit entry and return "denied", not "pending".
	adapter := newMockAdapter("mock.deny-wait", "run")
	env := newTestEnv(t, adapter)
	sc := newScenario(t, env, "bot")

	taskID := sc.createApprovedTask(t, env, "mock.deny-wait", "run", false)
	reqID := fmt.Sprintf("deny-wait-%s", randSuffix())

	// Start a wait=true gateway request in a goroutine.
	var (
		wg       sync.WaitGroup
		pollResp *http.Response
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		pollResp = env.do("POST", "/api/gateway/request?wait=true&timeout=10",
			sc.AgentToken, map[string]any{
				"service":    "mock.deny-wait",
				"action":     "run",
				"reason":     "test deny during wait",
				"request_id": reqID,
				"task_id":    taskID,
			})
	}()

	// Give the long-poll a moment to subscribe, then deny.
	time.Sleep(300 * time.Millisecond)
	resp := sc.session.do("POST", fmt.Sprintf("/api/approvals/%s/deny", reqID), nil)
	mustStatus(t, resp, http.StatusOK)

	wg.Wait()
	body := mustStatus(t, pollResp, http.StatusOK)
	if body["status"] != "denied" {
		t.Errorf("wait=true after deny: expected status=denied, got %v", body["status"])
	}
}

func TestGateway_Execute_DoubleExecution_Blocked(t *testing.T) {
	// Bug regression: two concurrent /execute calls on the same approved request
	// must not both execute the adapter. The atomic claim ensures only one wins.
	adapter := newMockAdapter("mock.double", "run").
		withResult("ok", nil)
	env := newTestEnv(t, adapter)
	sc := newScenario(t, env, "bot")

	taskID := sc.createApprovedTask(t, env, "mock.double", "run", false)
	reqID := fmt.Sprintf("double-exec-%s", randSuffix())
	sc.gatewayRequestWithTask(env, reqID, "mock.double", "run", taskID)

	// Approve.
	resp := sc.session.do("POST", fmt.Sprintf("/api/approvals/%s/approve", reqID), nil)
	mustStatus(t, resp, http.StatusOK)

	// Race two /execute calls.
	var wg sync.WaitGroup
	type result struct {
		status int
		body   map[string]any
	}
	results := make([]result, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			r := env.do("POST", fmt.Sprintf("/api/gateway/request/%s/execute", reqID), sc.AgentToken, nil)
			results[idx] = result{status: r.StatusCode, body: mustStatus(t, r, r.StatusCode)}
		}(i)
	}
	wg.Wait()

	// Exactly one should succeed with "executed". The other should be blocked —
	// either 409 Conflict (claim race) or 404 Not Found (row already deleted
	// after the winner finished executing).
	executed := 0
	blocked := 0
	for i := 0; i < 2; i++ {
		if results[i].status == http.StatusOK && results[i].body["status"] == "executed" {
			executed++
		} else if results[i].status == http.StatusConflict || results[i].status == http.StatusNotFound {
			blocked++
		}
	}
	if executed != 1 {
		t.Errorf("expected exactly 1 executed, got %d (results: %+v)", executed, results)
	}
	if blocked != 1 {
		t.Errorf("expected exactly 1 blocked (409 or 404), got %d (results: %+v)", blocked, results)
	}
}
