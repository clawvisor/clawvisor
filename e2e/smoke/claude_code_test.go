package smoke_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// longPollClient returns an HTTP client with a timeout long enough for
// ?wait=true long-poll requests (the server defaults to 120s).
func longPollClient() *http.Client {
	return &http.Client{Timeout: 150 * time.Second}
}

// doLongPoll is like doRaw but uses a long-poll client for ?wait=true requests.
func (e *e2eEnv) doLongPoll(method, path string, token string, body any) *http.Response {
	saved := e.client
	e.client = longPollClient()
	defer func() { e.client = saved }()
	return e.doRaw(method, path, token, body)
}

// approveConnectionInBackground lists pending connections and approves the one
// matching agentName. Must be called before the blocking ?wait=true POST.
// Returns a channel that receives the connection_id once approved.
func approveConnectionInBackground(t *testing.T, env *e2eEnv, agentName string) <-chan string {
	t.Helper()
	ch := make(chan string, 1)
	go func() {
		defer close(ch)
		for i := 0; i < 30; i++ {
			time.Sleep(500 * time.Millisecond)
			resp := env.userDo("GET", "/api/agents/connections", nil)
			defer resp.Body.Close()
			b, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != http.StatusOK {
				continue
			}
			var connections []map[string]any
			if err := json.Unmarshal(b, &connections); err != nil {
				continue
			}
			for _, c := range connections {
				name := strOr(c, "name", "")
				status := strOr(c, "status", "")
				id := strOr(c, "id", "")
				if name == agentName && status == "pending" && id != "" {
					approveResp := env.userDo("POST", fmt.Sprintf("/api/agents/connect/%s/approve", id), nil)
					approveResp.Body.Close()
					if approveResp.StatusCode == http.StatusOK {
						ch <- id
						return
					}
				}
			}
		}
	}()
	return ch
}

// approveTaskInBackground polls for a pending task owned by the given agent
// and approves it. Returns a channel that receives the task_id once approved.
func approveTaskInBackground(t *testing.T, env *e2eEnv, purpose string) <-chan string {
	t.Helper()
	ch := make(chan string, 1)
	go func() {
		defer close(ch)
		for i := 0; i < 30; i++ {
			time.Sleep(500 * time.Millisecond)
			resp := env.userDo("GET", "/api/tasks", nil)
			defer resp.Body.Close()
			b, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != http.StatusOK {
				continue
			}
			var m map[string]any
			if err := json.Unmarshal(b, &m); err != nil {
				continue
			}
			tasks, _ := m["tasks"].([]any)
			for _, raw := range tasks {
				task, ok := raw.(map[string]any)
				if !ok {
					continue
				}
				p := strOr(task, "purpose", "")
				s := strOr(task, "status", "")
				id := strOr(task, "id", "")
				if strings.Contains(p, purpose) && (s == "pending_approval" || s == "pending") && id != "" {
					approveResp := env.userDo("POST", fmt.Sprintf("/api/tasks/%s/approve", id), nil)
					approveResp.Body.Close()
					if approveResp.StatusCode == http.StatusOK {
						ch <- id
						return
					}
				}
			}
		}
	}()
	return ch
}

// TestClaudeCodeSetupFlow simulates the exact steps that /clawvisor-setup
// tells Claude Code to execute:
//
//  1. Verify daemon running (GET /ready)
//  2. Connect as "claude-code" agent with ?wait=true (long-poll)
//  3. Verify token by fetching catalog
//  4. Smoke test: in-scope read succeeds, out-of-scope request denied
//  5. Complete the task
func TestClaudeCodeSetupFlow(t *testing.T) {
	env := setup(t)
	svcID, action, params := env.pickActivatedReadService(t)

	// ── Step 1: Verify daemon running ─────────────────────────────────────
	readyResp := env.doRaw("GET", "/ready", "", nil)
	mustStatus(t, readyResp, http.StatusOK)
	t.Log("step 1: daemon is running")

	// ── Step 2: Connect as "claude-code" with ?wait=true ──────────────────
	// Start background approver before the blocking POST.
	agentName := fmt.Sprintf("claude-code-e2e-%d", time.Now().UnixNano())
	approvedCh := approveConnectionInBackground(t, env, agentName)

	connResp := env.doLongPoll("POST", "/api/agents/connect?wait=true&timeout=30", "", map[string]any{
		"name":        agentName,
		"description": "Claude Code agent (e2e test)",
	})
	connM := mustStatus(t, connResp, http.StatusCreated)
	connStatus := strOr(connM, "status", "")
	if connStatus != "approved" {
		t.Fatalf("expected approved, got %s", connStatus)
	}
	claudeToken := strOr(connM, "token", "")
	if claudeToken == "" {
		t.Fatal("expected token in ?wait=true response")
	}
	if !strings.HasPrefix(claudeToken, "cvis_") {
		t.Errorf("token missing cvis_ prefix: %q", claudeToken[:10])
	}
	t.Logf("step 2: connected as %s, token=%d chars", agentName, len(claudeToken))

	// Drain the approval channel.
	<-approvedCh

	claudeReq := func(method, path string, body any) *http.Response {
		return env.doRaw(method, path, claudeToken, body)
	}
	claudeLongPoll := func(method, path string, body any) *http.Response {
		return env.doLongPoll(method, path, claudeToken, body)
	}

	// ── Step 3: Verify token via catalog ──────────────────────────────────
	catalogResp := claudeReq("GET", "/api/skill/catalog", nil)
	defer catalogResp.Body.Close()
	if catalogResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(catalogResp.Body)
		t.Fatalf("catalog: status %d: %s", catalogResp.StatusCode, body)
	}
	catalogBody, _ := io.ReadAll(catalogResp.Body)
	if !strings.Contains(string(catalogBody), "Service Catalog") {
		t.Error("catalog response missing expected header")
	}
	t.Logf("step 3: catalog verified (%d bytes)", len(catalogBody))

	// ── Step 4: Smoke test — create task with ?wait=true ──────────────────
	purposeMarker := fmt.Sprintf("setup-smoke-%d", time.Now().UnixNano())
	taskApprovedCh := approveTaskInBackground(t, env, purposeMarker)

	taskResp := claudeLongPoll("POST", "/api/tasks?wait=true&timeout=30", map[string]any{
		"purpose": fmt.Sprintf("Clawvisor setup smoke test [%s]: read %s data", purposeMarker, svcID),
		"authorized_actions": []map[string]any{{
			"service":      svcID,
			"action":       action,
			"auto_execute": true,
			"expected_use": "Setup smoke test — verify in-scope read works",
		}},
	})
	taskM := mustStatus(t, taskResp, http.StatusCreated)
	taskID := strOr(taskM, "id", strOr(taskM, "task_id", ""))
	if taskID == "" {
		t.Fatal("no task_id in response")
	}
	taskStatus := strOr(taskM, "status", "")
	if taskStatus != "approved" && taskStatus != "active" {
		t.Fatalf("expected task approved/active after wait, got %s", taskStatus)
	}
	t.Logf("step 4a: task %s created and approved via ?wait=true", taskID)
	<-taskApprovedCh

	// In-scope request (should succeed with auto_execute=true).
	reqID := fmt.Sprintf("e2e-setup-inscope-%d", time.Now().UnixNano())
	gwResp := claudeLongPoll("POST", "/api/gateway/request?wait=true&timeout=30", map[string]any{
		"service":    svcID,
		"action":     action,
		"params":     params,
		"reason":     "Setup smoke test — in-scope read",
		"request_id": reqID,
		"task_id":    taskID,
	})
	gwM := parseGatewayResponse(t, gwResp)
	gwStatus := str(t, gwM, "status")
	t.Logf("step 4b: in-scope request status=%s", gwStatus)

	switch gwStatus {
	case "executed", "completed", "approved", "auto_approved":
		// Success path.
	case "restricted":
		t.Log("in-scope request restricted by intent verification (non-fatal)")
	case "blocked":
		t.Log("in-scope request blocked by restriction rule (non-fatal)")
	default:
		t.Errorf("unexpected in-scope status: %s", gwStatus)
	}

	// Out-of-scope request (different action not in the task).
	secondAction := env.pickSecondAction(t, svcID, action)
	if secondAction != "" {
		oosReqID := fmt.Sprintf("e2e-setup-oos-%d", time.Now().UnixNano())
		oosResp := claudeReq("POST", "/api/gateway/request", map[string]any{
			"service":    svcID,
			"action":     secondAction,
			"params":     map[string]any{},
			"reason":     "Setup smoke test — out-of-scope (should be denied)",
			"request_id": oosReqID,
			"task_id":    taskID,
		})
		defer oosResp.Body.Close()
		oosBody, _ := io.ReadAll(oosResp.Body)
		var oosM map[string]any
		if err := json.Unmarshal(oosBody, &oosM); err == nil {
			oosStatus := strOr(oosM, "status", "")
			if oosStatus == "executed" || oosStatus == "auto_approved" {
				t.Errorf("out-of-scope request should not succeed, got status=%s", oosStatus)
			} else {
				t.Logf("step 4c: out-of-scope request correctly returned status=%s", oosStatus)
			}
		} else {
			t.Logf("step 4c: out-of-scope request returned HTTP %d (non-200 expected)", oosResp.StatusCode)
		}
	} else {
		t.Log("step 4c: skipped out-of-scope test (no second action available)")
	}

	// ── Step 5: Complete the task ─────────────────────────────────────────
	completeResp := claudeReq("POST", fmt.Sprintf("/api/tasks/%s/complete", taskID), nil)
	defer completeResp.Body.Close()
	if completeResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(completeResp.Body)
		t.Fatalf("complete: status %d: %s", completeResp.StatusCode, body)
	}
	t.Log("step 5: task completed — Claude Code setup flow passed")
}

// TestClaudeCodeSkillFlow simulates how Claude Code uses the Clawvisor skill
// after setup is complete, following the "Typical Flow" from SKILL.md:
// catalog → task(?wait=true) → gateway(?wait=true) → complete.
func TestClaudeCodeSkillFlow(t *testing.T) {
	env := setup(t)
	svcID, action, params := env.pickActivatedReadService(t)

	// Use the harness agent token (simulates an already-connected Claude Code).
	agentToken := env.AgentToken

	agentReq := func(method, path string, body any) *http.Response {
		return env.doRaw(method, path, agentToken, body)
	}
	agentLongPoll := func(method, path string, body any) *http.Response {
		return env.doLongPoll(method, path, agentToken, body)
	}

	// ── Fetch catalog ─────────────────────────────────────────────────────
	catalogResp := agentReq("GET", "/api/skill/catalog", nil)
	defer catalogResp.Body.Close()
	if catalogResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(catalogResp.Body)
		t.Fatalf("catalog: status %d: %s", catalogResp.StatusCode, body)
	}
	catalogBody, _ := io.ReadAll(catalogResp.Body)
	if !strings.Contains(string(catalogBody), svcID) {
		t.Fatalf("catalog missing %s", svcID)
	}
	t.Logf("catalog fetched, found %s (%d bytes)", svcID, len(catalogBody))

	// ── Create task with ?wait=true ───────────────────────────────────────
	purposeMarker := fmt.Sprintf("skill-flow-%d", time.Now().UnixNano())
	taskApprovedCh := approveTaskInBackground(t, env, purposeMarker)

	taskResp := agentLongPoll("POST", "/api/tasks?wait=true&timeout=30", map[string]any{
		"purpose": fmt.Sprintf("Skill flow test [%s]: %s/%s", purposeMarker, svcID, action),
		"authorized_actions": []map[string]any{{
			"service":      svcID,
			"action":       action,
			"auto_execute": true,
			"expected_use": "Skill flow test — read action",
		}},
	})
	taskM := mustStatus(t, taskResp, http.StatusCreated)
	taskID := strOr(taskM, "id", strOr(taskM, "task_id", ""))
	if taskID == "" {
		t.Fatal("no task_id in response")
	}
	taskStatus := strOr(taskM, "status", "")
	if taskStatus != "approved" && taskStatus != "active" {
		t.Fatalf("expected approved/active after wait, got %s", taskStatus)
	}
	t.Logf("task %s approved via ?wait=true", taskID)
	<-taskApprovedCh

	// ── Gateway request with ?wait=true ───────────────────────────────────
	reqID := fmt.Sprintf("e2e-skill-%d", time.Now().UnixNano())
	gwResp := agentLongPoll("POST", "/api/gateway/request?wait=true&timeout=30", map[string]any{
		"service":    svcID,
		"action":     action,
		"params":     params,
		"reason":     fmt.Sprintf("Skill flow test: %s/%s", svcID, action),
		"request_id": reqID,
		"task_id":    taskID,
	})
	gwM := parseGatewayResponse(t, gwResp)
	gwStatus := str(t, gwM, "status")
	t.Logf("gateway status=%s", gwStatus)
	env.executeIfReady(t, reqID, gwStatus, gwM)

	// ── Complete task ─────────────────────────────────────────────────────
	completeResp := agentReq("POST", fmt.Sprintf("/api/tasks/%s/complete", taskID), nil)
	defer completeResp.Body.Close()
	if completeResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(completeResp.Body)
		t.Fatalf("complete: status %d: %s", completeResp.StatusCode, body)
	}

	// Verify final status.
	finalResp := agentReq("GET", fmt.Sprintf("/api/tasks/%s", taskID), nil)
	finalM := mustStatus(t, finalResp, http.StatusOK)
	if strOr(finalM, "status", "") != "completed" {
		t.Errorf("expected completed, got %s", strOr(finalM, "status", ""))
	}
	t.Log("task completed — skill flow passed")
}

// TestClaudeCodeOutOfScopeBlocked verifies that the out-of-scope
// demonstration from /clawvisor-setup step 5 works correctly: a gateway
// request for an action not in the task's authorized_actions is rejected.
func TestClaudeCodeOutOfScopeBlocked(t *testing.T) {
	env := setup(t)
	svcID, action, _ := env.pickActivatedReadService(t)

	secondAction := env.pickSecondAction(t, svcID, action)
	if secondAction == "" {
		t.Skipf("service %s has no second action for out-of-scope test", svcID)
	}

	// Create task scoped to only the first action.
	taskID := env.createApprovedTask(t, svcID, action, true)

	// Request the second action (not in scope).
	reqID := fmt.Sprintf("e2e-oos-%d", time.Now().UnixNano())
	resp := env.agentDo("POST", "/api/gateway/request", map[string]any{
		"service":    svcID,
		"action":     secondAction,
		"params":     map[string]any{},
		"reason":     "Out-of-scope test — should be rejected",
		"request_id": reqID,
		"task_id":    taskID,
	})
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)

	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Logf("out-of-scope returned HTTP %d (body not JSON)", resp.StatusCode)
		if resp.StatusCode == http.StatusOK {
			t.Error("expected non-success for out-of-scope request")
		}
		return
	}

	status := strOr(m, "status", "")
	t.Logf("out-of-scope status=%s (HTTP %d)", status, resp.StatusCode)

	// The server should return pending_scope_expansion, blocked, or a non-success status.
	switch status {
	case "pending_scope_expansion":
		t.Log("correctly triggered scope expansion requirement")
	case "blocked", "restricted", "denied", "pending", "pending_approval":
		t.Logf("correctly rejected with status=%s", status)
	case "executed", "auto_approved", "completed":
		t.Errorf("out-of-scope request should not succeed, got status=%s", status)
	default:
		t.Logf("unexpected status=%s — verifying it's not a success", status)
	}
}

// TestClaudeCodeWaitTrueTaskGet tests the ?wait=true long-poll on
// GET /api/tasks/{id}, which Claude Code uses when a POST /api/tasks?wait=true
// times out and it needs to resume polling.
func TestClaudeCodeWaitTrueTaskGet(t *testing.T) {
	env := setup(t)
	svcID, action, _ := env.pickActivatedReadService(t)

	// Create a task without ?wait=true so we can test the GET long-poll.
	taskResp := env.agentDo("POST", "/api/tasks", map[string]any{
		"purpose": "wait-true task get test",
		"authorized_actions": []map[string]any{{
			"service":      svcID,
			"action":       action,
			"auto_execute": true,
		}},
	})
	taskM := mustStatus(t, taskResp, http.StatusCreated)
	taskID := str(t, taskM, "task_id")
	t.Logf("task %s created, testing GET ?wait=true", taskID)

	// Approve from background while we long-poll.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 15; i++ {
			time.Sleep(500 * time.Millisecond)
			checkResp := env.agentDo("GET", fmt.Sprintf("/api/tasks/%s", taskID), nil)
			checkM := mustStatus(t, checkResp, http.StatusOK)
			s := strOr(checkM, "status", "")
			if s == "pending_approval" || s == "pending" {
				approveResp := env.userDo("POST", fmt.Sprintf("/api/tasks/%s/approve", taskID), nil)
				approveResp.Body.Close()
				return
			}
			if s == "approved" || s == "active" {
				return
			}
		}
	}()

	// Long-poll GET until approved.
	getResp := env.doLongPoll("GET", fmt.Sprintf("/api/tasks/%s?wait=true&timeout=30", taskID), env.AgentToken, nil)
	getM := mustStatus(t, getResp, http.StatusOK)
	s := strOr(getM, "status", "")
	if s != "approved" && s != "active" {
		t.Errorf("expected approved/active after wait, got %s", s)
	}
	t.Logf("GET ?wait=true returned status=%s", s)

	wg.Wait()
}
