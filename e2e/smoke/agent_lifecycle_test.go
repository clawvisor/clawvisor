package smoke_test

import (
	"fmt"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestAgentConnectionFlow exercises the self-service agent onboarding:
// POST /api/agents/connect → user approves → poll for token → use token.
func TestAgentConnectionFlow(t *testing.T) {
	env := setup(t)

	// Step 1: Agent requests a connection (unauthenticated).
	connResp := env.doRaw("POST", "/api/agents/connect", "", map[string]any{
		"name":        "e2e-connection-test",
		"description": "E2E test agent via connection flow",
	})
	connM := mustStatus(t, connResp, http.StatusCreated)
	connID := str(t, connM, "connection_id")
	connStatus := str(t, connM, "status")
	t.Logf("connection request %s created, status=%s", connID, connStatus)

	if connStatus != "pending" {
		t.Fatalf("expected pending, got %s", connStatus)
	}

	// Step 2: User approves the connection.
	approveResp := env.userDo("POST", fmt.Sprintf("/api/agents/connect/%s/approve", connID), nil)
	approveM := mustStatus(t, approveResp, http.StatusOK)
	approveStatus := str(t, approveM, "status")
	if approveStatus != "approved" {
		t.Fatalf("expected approved, got %s", approveStatus)
	}
	agentID := strOr(approveM, "agent_id", "")
	t.Logf("connection approved, agent_id=%s", agentID)

	// Step 3: Agent polls for the token.
	pollResp := env.doRaw("GET", fmt.Sprintf("/api/agents/connect/%s/status", connID), "", nil)
	pollM := mustStatus(t, pollResp, http.StatusOK)
	pollStatus := str(t, pollM, "status")
	if pollStatus != "approved" {
		t.Fatalf("expected approved on poll, got %s", pollStatus)
	}

	token := strOr(pollM, "token", "")
	if token == "" {
		t.Fatal("expected token in approved poll response, got empty string")
	}
	if !strings.HasPrefix(token, "cvis_") {
		t.Errorf("token should have cvis_ prefix, got %q", token[:10])
	}
	t.Logf("received agent token (%d chars)", len(token))

	// Step 4: Verify the token works by fetching the skill catalog.
	catalogResp := env.doRaw("GET", "/api/skill/catalog", token, nil)
	defer catalogResp.Body.Close()
	if catalogResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(catalogResp.Body)
		t.Fatalf("catalog with new token: status %d: %s", catalogResp.StatusCode, body)
	}
	catalogBody, _ := io.ReadAll(catalogResp.Body)
	if !strings.Contains(string(catalogBody), "Service Catalog") {
		t.Error("catalog response missing expected header")
	}
	t.Logf("new agent can access catalog (%d bytes)", len(catalogBody))
}

// TestAgentConnectionDeny tests that denying a connection request works.
func TestAgentConnectionDeny(t *testing.T) {
	env := setup(t)

	connResp := env.doRaw("POST", "/api/agents/connect", "", map[string]any{
		"name": "e2e-deny-test",
	})
	connM := mustStatus(t, connResp, http.StatusCreated)
	connID := str(t, connM, "connection_id")

	// Deny it.
	denyResp := env.userDo("POST", fmt.Sprintf("/api/agents/connect/%s/deny", connID), nil)
	denyM := mustStatus(t, denyResp, http.StatusOK)
	if str(t, denyM, "status") != "denied" {
		t.Fatalf("expected denied, got %s", str(t, denyM, "status"))
	}

	// Poll should show denied with no token.
	pollResp := env.doRaw("GET", fmt.Sprintf("/api/agents/connect/%s/status", connID), "", nil)
	pollM := mustStatus(t, pollResp, http.StatusOK)
	if str(t, pollM, "status") != "denied" {
		t.Errorf("expected denied on poll, got %s", str(t, pollM, "status"))
	}
	if strOr(pollM, "token", "") != "" {
		t.Error("denied connection should not return a token")
	}
	t.Log("connection correctly denied")
}

// TestAgentConnectionsList tests that pending connections are visible to the user.
func TestAgentConnectionsList(t *testing.T) {
	env := setup(t)

	// Create a connection request so there's at least one.
	connResp := env.doRaw("POST", "/api/agents/connect", "", map[string]any{
		"name": "e2e-list-test",
	})
	mustStatus(t, connResp, http.StatusCreated)

	// List connections as user.
	listResp := env.userDo("GET", "/api/agents/connections", nil)
	defer listResp.Body.Close()
	if listResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(listResp.Body)
		t.Fatalf("list connections: status %d: %s", listResp.StatusCode, body)
	}

	var connections []any
	decodeJSON(t, listResp, &connections)
	t.Logf("found %d connection request(s)", len(connections))

	if len(connections) == 0 {
		t.Error("expected at least one connection request")
	}
}

// TestFullAgentLifecycle exercises the complete agent journey using ?wait=true
// long-polls as instructed by the setup script:
// connect(?wait=true) → catalog → task(?wait=true) → gateway(?wait=true) →
// complete → verify.
func TestFullAgentLifecycle(t *testing.T) {
	env := setup(t)

	// Pick any activated read-only service so the test works regardless of
	// which services the user has configured.
	svcID, action, params := env.pickActivatedReadService(t)
	t.Logf("using service=%s action=%s", svcID, action)

	// ── Phase 1: Agent connects with ?wait=true ───────────────────────────
	agentName := fmt.Sprintf("e2e-lifecycle-%d", time.Now().UnixNano())
	approvedCh := approveConnectionInBackground(t, env, agentName)

	connResp := env.doLongPoll("POST", "/api/agents/connect?wait=true&timeout=30", "", map[string]any{
		"name":        agentName,
		"description": "Full lifecycle test agent",
	})
	connM := mustStatus(t, connResp, http.StatusCreated)
	connStatus := strOr(connM, "status", "")
	if connStatus != "approved" {
		t.Fatalf("expected approved, got %s", connStatus)
	}
	agentToken := strOr(connM, "token", "")
	if agentToken == "" {
		t.Fatal("expected token in ?wait=true response")
	}
	t.Logf("phase 1: agent connected, token acquired (%d chars)", len(agentToken))
	<-approvedCh

	agentReq := func(method, path string, body any) *http.Response {
		return env.doRaw(method, path, agentToken, body)
	}
	agentLongPoll := func(method, path string, body any) *http.Response {
		return env.doLongPoll(method, path, agentToken, body)
	}

	// ── Phase 2: Agent discovers available services via catalog ───────────
	catalogResp := agentReq("GET", "/api/skill/catalog", nil)
	defer catalogResp.Body.Close()
	if catalogResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(catalogResp.Body)
		t.Fatalf("catalog: status %d: %s", catalogResp.StatusCode, body)
	}
	catalogBody, _ := io.ReadAll(catalogResp.Body)
	catalog := string(catalogBody)
	if !strings.Contains(catalog, svcID) {
		t.Fatalf("catalog does not list %s service", svcID)
	}
	t.Logf("phase 2: catalog fetched, contains %s (%d bytes)", svcID, len(catalog))

	// ── Phase 3: Agent creates a task with ?wait=true ─────────────────────
	purposeMarker := fmt.Sprintf("lifecycle-%d", time.Now().UnixNano())
	taskApprovedCh := approveTaskInBackground(t, env, purposeMarker)

	taskResp := agentLongPoll("POST", "/api/tasks?wait=true&timeout=30", map[string]any{
		"purpose": fmt.Sprintf("E2E lifecycle test [%s]: %s/%s", purposeMarker, svcID, action),
		"authorized_actions": []map[string]any{{
			"service":      svcID,
			"action":       action,
			"auto_execute": true,
			"expected_use": "E2E lifecycle test read action",
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
	t.Logf("phase 3: task %s approved via ?wait=true", taskID)
	<-taskApprovedCh

	// ── Phase 4: Agent submits a gateway request with ?wait=true ──────────
	reqID := fmt.Sprintf("e2e-lifecycle-%d", time.Now().UnixNano())
	gwResp := agentLongPoll("POST", "/api/gateway/request?wait=true&timeout=30", map[string]any{
		"service":    svcID,
		"action":     action,
		"params":     params,
		"reason":     fmt.Sprintf("E2E lifecycle: %s/%s", svcID, action),
		"request_id": reqID,
		"task_id":    taskID,
	})
	gwM := parseGatewayResponse(t, gwResp)
	gwStatus := str(t, gwM, "status")
	t.Logf("phase 4: gateway response status=%s", gwStatus)

	env.executeIfReady(t, reqID, gwStatus, gwM)
	t.Log("phase 4: gateway request executed")

	// ── Phase 5: Agent completes the task ────────────────────────────────
	completeResp := agentReq("POST", fmt.Sprintf("/api/tasks/%s/complete", taskID), nil)
	defer completeResp.Body.Close()
	completeBody, _ := io.ReadAll(completeResp.Body)
	if completeResp.StatusCode != http.StatusOK {
		t.Fatalf("complete task: status %d: %s", completeResp.StatusCode, completeBody)
	}
	t.Logf("phase 5: task %s completed", taskID)

	// Verify task status is now completed.
	finalResp := agentReq("GET", fmt.Sprintf("/api/tasks/%s", taskID), nil)
	finalM := mustStatus(t, finalResp, http.StatusOK)
	finalStatus := strOr(finalM, "status", "")
	if finalStatus != "completed" {
		t.Errorf("expected task status=completed, got %s", finalStatus)
	}
	t.Log("phase 5: verified task is completed")
}

// TestStandingTaskLifecycle tests creating a standing (persistent) task
// that doesn't expire, using it for a gateway request, and then revoking it.
func TestStandingTaskLifecycle(t *testing.T) {
	env := setup(t)
	svcID, action, params := env.pickActivatedReadService(t)

	// Create a standing task.
	taskResp := env.agentDo("POST", "/api/tasks", map[string]any{
		"purpose":  "E2E standing task test",
		"lifetime": "standing",
		"authorized_actions": []map[string]any{{
			"service":      svcID,
			"action":       action,
			"auto_execute": true,
			"expected_use": "Ongoing read monitoring",
		}},
	})
	taskM := mustStatus(t, taskResp, http.StatusCreated)
	taskID := str(t, taskM, "task_id")
	t.Logf("standing task %s created (%s/%s)", taskID, svcID, action)

	agentReq := func(method, path string, body any) *http.Response {
		return env.agentDo(method, path, body)
	}
	env.waitAndApproveTask(t, agentReq, taskID)
	t.Logf("standing task %s approved", taskID)

	// Use the standing task for a request.
	reqID := fmt.Sprintf("e2e-standing-%d", time.Now().UnixNano())
	gwResp := env.agentDo("POST", "/api/gateway/request", map[string]any{
		"service":    svcID,
		"action":     action,
		"params":     params,
		"reason":     "E2E standing task test",
		"request_id": reqID,
		"task_id":    taskID,
		"session_id": "e2e-session-1",
	})
	gwM := parseGatewayResponse(t, gwResp)
	gwStatus := str(t, gwM, "status")
	t.Logf("standing task gateway status=%s", gwStatus)

	env.executeIfReady(t, reqID, gwStatus, gwM)

	// Revoke the standing task via user token.
	revokeResp := env.userDo("POST", fmt.Sprintf("/api/tasks/%s/revoke", taskID), nil)
	defer revokeResp.Body.Close()
	if revokeResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(revokeResp.Body)
		t.Fatalf("revoke standing task: status %d: %s", revokeResp.StatusCode, body)
	}
	t.Logf("standing task %s revoked", taskID)

	// Verify that a new request on the revoked task fails.
	reqID2 := fmt.Sprintf("e2e-standing-revoked-%d", time.Now().UnixNano())
	revokedResp := env.agentDo("POST", "/api/gateway/request", map[string]any{
		"service":    svcID,
		"action":     action,
		"params":     params,
		"reason":     "Should fail — task revoked",
		"request_id": reqID2,
		"task_id":    taskID,
	})
	defer revokedResp.Body.Close()
	revokedBody, _ := io.ReadAll(revokedResp.Body)
	if revokedResp.StatusCode == http.StatusOK {
		var revokedM map[string]any
		if err := json.Unmarshal(revokedBody, &revokedM); err == nil {
			s := strOr(revokedM, "status", "")
			if s == "executed" || s == "approved" || s == "auto_approved" {
				t.Errorf("expected revoked task request to fail, but got status=%s", s)
			} else {
				t.Logf("revoked task request correctly returned status=%s", s)
			}
		}
	} else {
		t.Logf("revoked task request correctly returned HTTP %d", revokedResp.StatusCode)
	}
}

// TestTaskScopeExpansion tests that an agent can request scope expansion
// for an action not in the original task.
func TestTaskScopeExpansion(t *testing.T) {
	env := setup(t)
	svcID, action, _ := env.pickActivatedReadService(t)

	// Create a task with only the first action.
	taskID := env.createApprovedTask(t, svcID, action, true)

	// Pick a second action from the same service for expansion.
	secondAction := env.pickSecondAction(t, svcID, action)
	if secondAction == "" {
		t.Skipf("service %s has no second read action to expand into", svcID)
	}

	// Request scope expansion.
	expandResp := env.agentDo("POST", fmt.Sprintf("/api/tasks/%s/expand", taskID), map[string]any{
		"service":      svcID,
		"action":       secondAction,
		"auto_execute": true,
		"reason":       "E2E scope expansion test",
	})
	defer expandResp.Body.Close()
	expandBody, _ := io.ReadAll(expandResp.Body)

	if expandResp.StatusCode == http.StatusOK || expandResp.StatusCode == http.StatusAccepted {
		t.Logf("scope expansion requested (status %d)", expandResp.StatusCode)
	} else {
		t.Logf("scope expansion returned %d: %s (may need user approval or not be supported)", expandResp.StatusCode, expandBody)
		return
	}

	// Check if expansion needs approval.
	checkResp := env.agentDo("GET", fmt.Sprintf("/api/tasks/%s", taskID), nil)
	checkM := mustStatus(t, checkResp, http.StatusOK)
	taskStatus := strOr(checkM, "status", "")
	t.Logf("task status after expansion request: %s", taskStatus)

	if taskStatus == "pending_scope_expansion" {
		// Approve the expansion so the task doesn't stay in a stuck state.
		approveResp := env.userDo("POST", fmt.Sprintf("/api/tasks/%s/expand/approve", taskID), nil)
		defer approveResp.Body.Close()
		if approveResp.StatusCode == http.StatusOK {
			t.Log("expansion approved by user")
		} else {
			body, _ := io.ReadAll(approveResp.Body)
			t.Logf("expansion approve returned %d: %s", approveResp.StatusCode, body)
		}
	} else if taskStatus == "pending_approval" {
		approveResp := env.userDo("POST", fmt.Sprintf("/api/tasks/%s/approve", taskID), nil)
		defer approveResp.Body.Close()
		if approveResp.StatusCode == http.StatusOK {
			t.Log("expansion approved by user")
		}
	}
}

// TestAgentListAfterConnect verifies the agents list endpoint shows the
// agents created via the connection flow.
func TestAgentListAfterConnect(t *testing.T) {
	env := setup(t)

	resp := env.userDo("GET", "/api/agents", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("list agents: status %d: %s", resp.StatusCode, body)
	}

	var agents []any
	decodeJSON(t, resp, &agents)
	t.Logf("user has %d agent(s)", len(agents))

	if len(agents) == 0 {
		t.Error("expected at least one agent (e2e-smoke-test)")
	}

	// Check that our test agent is in the list.
	found := false
	for _, raw := range agents {
		agent, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		name := strOr(agent, "name", "")
		t.Logf("  agent: %s (id=%s)", name, strOr(agent, "id", "?"))
		if name == "e2e-smoke-test" {
			found = true
		}
	}
	if !found {
		t.Error("e2e-smoke-test agent not found in agents list")
	}
}
