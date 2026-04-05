package smoke_test

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestSkillDocument verifies the rendered skill document is served and
// contains the essential sections an agent needs to learn how to interact.
func TestSkillDocument(t *testing.T) {
	env := setup(t)

	// /skill/SKILL.md serves the rendered template (claude-code target).
	resp := env.doRaw("GET", "/skill/SKILL.md", "", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET /skill/SKILL.md: status %d: %s", resp.StatusCode, body)
	}

	body, _ := io.ReadAll(resp.Body)
	skill := string(body)
	t.Logf("SKILL.md: %d bytes", len(skill))

	// Verify the skill document teaches agents the core API flow.
	required := []struct {
		section string
		marker  string
	}{
		{"service catalog endpoint", "/api/skill/catalog"},
		{"task creation endpoint", "/api/tasks"},
		{"gateway endpoint", "/api/gateway/request"},
		{"task completion", "/complete"},
		{"authorization model", "auto_execute"},
		{"agent token env var", "CLAWVISOR_AGENT_TOKEN"},
		{"server URL env var", "CLAWVISOR_URL"},
	}

	for _, r := range required {
		if !strings.Contains(skill, r.marker) {
			t.Errorf("SKILL.md missing %s (expected %q)", r.section, r.marker)
		}
	}
}

// TestSetupDocument verifies the /skill/setup endpoint returns a
// pre-filled onboarding document with the server URL.
func TestSetupDocument(t *testing.T) {
	env := setup(t)

	resp := env.doRaw("GET", "/skill/setup", "", nil)
	defer resp.Body.Close()

	// The setup endpoint may return 404 if no daemon_id is configured,
	// which is expected in local test mode.
	if resp.StatusCode == http.StatusNotFound {
		t.Log("GET /skill/setup returned 404 (no daemon_id configured — expected in local mode)")
		return
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET /skill/setup: status %d: %s", resp.StatusCode, body)
	}

	body, _ := io.ReadAll(resp.Body)
	setup := string(body)
	t.Logf("setup document: %d bytes", len(setup))

	if !strings.Contains(setup, "CLAWVISOR_URL") {
		t.Error("setup document missing CLAWVISOR_URL")
	}
}

// TestSkillZipBundle verifies the /skill/skill.zip bundle contains
// both SKILL.md and e2e.mjs.
func TestSkillZipBundle(t *testing.T) {
	env := setup(t)

	resp := env.doRaw("GET", "/skill/skill.zip", "", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET /skill/skill.zip: status %d: %s", resp.StatusCode, body)
	}

	body, _ := io.ReadAll(resp.Body)
	r, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatalf("invalid zip: %v", err)
	}

	files := map[string]bool{}
	for _, f := range r.File {
		files[f.Name] = true
		t.Logf("  zip entry: %s (%d bytes)", f.Name, f.UncompressedSize64)
	}

	if !files["SKILL.md"] {
		t.Error("skill.zip missing SKILL.md")
	}
	if !files["e2e.mjs"] {
		t.Error("skill.zip missing e2e.mjs")
	}
}

// TestE2EHelper verifies the e2e.mjs encryption helper is served
// and looks like valid JavaScript.
func TestE2EHelper(t *testing.T) {
	env := setup(t)

	resp := env.doRaw("GET", "/skill/e2e.mjs", "", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET /skill/e2e.mjs: status %d: %s", resp.StatusCode, body)
	}

	body, _ := io.ReadAll(resp.Body)
	js := string(body)
	t.Logf("e2e.mjs: %d bytes", len(js))

	// Verify it contains the expected exports.
	expected := []string{
		"createClient",
		"x25519",
		"aes-256-gcm",
		"clawvisor-e2e",
	}
	for _, e := range expected {
		if !strings.Contains(js, e) {
			t.Errorf("e2e.mjs missing expected content: %q", e)
		}
	}
}

// TestWellKnownKeys verifies the public key discovery endpoint.
func TestWellKnownKeys(t *testing.T) {
	env := setup(t)

	resp := env.doRaw("GET", "/.well-known/clawvisor-keys", "", nil)
	defer resp.Body.Close()

	// Returns 404 when no daemon keys are configured (local mode with relay disabled).
	if resp.StatusCode == http.StatusNotFound {
		t.Log("/.well-known/clawvisor-keys returned 404 (relay disabled — expected)")
		return
	}

	m := mustStatus(t, resp, http.StatusOK)
	if daemonID := strOr(m, "daemon_id", ""); daemonID != "" {
		t.Logf("daemon_id: %s", daemonID)
	}
	if algo := strOr(m, "algorithm", ""); algo != "" {
		t.Logf("algorithm: %s", algo)
	}
	if key := strOr(m, "x25519", ""); key != "" {
		t.Logf("x25519 public key: %s... (%d chars)", key[:16], len(key))
	}
}

// TestAgentDiscoveryToExecution exercises the full path an agent would take
// from reading the skill document through executing a real API request,
// using ?wait=true long-polls as instructed by the setup script and skill doc:
//
//  1. Fetch /skill/SKILL.md — learn the API
//  2. POST /api/agents/connect?wait=true — request access and block until approved
//  3. GET /api/skill/catalog — discover available services (as taught by skill doc)
//  4. POST /api/tasks?wait=true — create scoped task and block until approved
//  5. POST /api/gateway/request?wait=true — execute read action
//  6. POST /api/tasks/{id}/complete — finish
func TestAgentDiscoveryToExecution(t *testing.T) {
	env := setup(t)
	svcID, action, params := env.pickActivatedReadService(t)

	// ── Step 1: Agent reads the skill document ────────────────────────────
	skillResp := env.doRaw("GET", "/skill/SKILL.md", "", nil)
	defer skillResp.Body.Close()
	if skillResp.StatusCode != http.StatusOK {
		t.Fatalf("skill doc: status %d", skillResp.StatusCode)
	}
	skillBody, _ := io.ReadAll(skillResp.Body)
	skill := string(skillBody)

	// Verify the skill doc teaches the core API flow.
	if !strings.Contains(skill, "/api/skill/catalog") {
		t.Fatal("skill doc does not mention /api/skill/catalog")
	}
	if !strings.Contains(skill, "/api/tasks") {
		t.Fatal("skill doc does not mention /api/tasks")
	}
	if !strings.Contains(skill, "/api/gateway/request") {
		t.Fatal("skill doc does not mention /api/gateway/request")
	}
	t.Logf("step 1: read skill document (%d bytes), found all endpoint references", len(skill))

	// ── Step 2: Agent connects with ?wait=true (blocks until approved) ────
	agentName := fmt.Sprintf("e2e-discovery-%d", time.Now().UnixNano())
	approvedCh := approveConnectionInBackground(t, env, agentName)

	connResp := env.doLongPoll("POST", "/api/agents/connect?wait=true&timeout=30", "", map[string]any{
		"name":        agentName,
		"description": "Agent following setup doc instructions",
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
	t.Logf("step 2: connected and received token (%d chars)", len(agentToken))
	<-approvedCh

	agentReq := func(method, path string, body any) *http.Response {
		return env.doRaw(method, path, agentToken, body)
	}
	agentLongPoll := func(method, path string, body any) *http.Response {
		return env.doLongPoll(method, path, agentToken, body)
	}

	// ── Step 3: Agent fetches catalog (as instructed by skill doc) ────────
	catalogResp := agentReq("GET", "/api/skill/catalog", nil)
	defer catalogResp.Body.Close()
	if catalogResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(catalogResp.Body)
		t.Fatalf("catalog: status %d: %s", catalogResp.StatusCode, body)
	}
	catalogBody, _ := io.ReadAll(catalogResp.Body)
	catalog := string(catalogBody)
	if !strings.Contains(catalog, svcID) {
		t.Fatalf("catalog missing %s", svcID)
	}
	t.Logf("step 3: fetched catalog, found %s (%d bytes)", svcID, len(catalog))

	// ── Step 4: Agent creates a task with ?wait=true ──────────────────────
	purposeMarker := fmt.Sprintf("discovery-%d", time.Now().UnixNano())
	taskApprovedCh := approveTaskInBackground(t, env, purposeMarker)

	taskResp := agentLongPoll("POST", "/api/tasks?wait=true&timeout=30", map[string]any{
		"purpose": fmt.Sprintf("Discovery test [%s]: read %s data", purposeMarker, svcID),
		"authorized_actions": []map[string]any{{
			"service":      svcID,
			"action":       action,
			"auto_execute": true,
			"expected_use": "Following skill doc instructions for read-only access",
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
	t.Logf("step 4: task %s approved via ?wait=true", taskID)
	<-taskApprovedCh

	// ── Step 5: Agent makes a gateway request with ?wait=true ─────────────
	reqID := fmt.Sprintf("e2e-discovery-%d", time.Now().UnixNano())
	gwResp := agentLongPoll("POST", "/api/gateway/request?wait=true&timeout=30", map[string]any{
		"service":    svcID,
		"action":     action,
		"params":     params,
		"reason":     "Discovery test: reading data as instructed by skill doc",
		"request_id": reqID,
		"task_id":    taskID,
	})
	gwM := parseGatewayResponse(t, gwResp)
	gwStatus := str(t, gwM, "status")
	t.Logf("step 5: gateway status=%s", gwStatus)
	env.executeIfReady(t, reqID, gwStatus, gwM)

	// ── Step 6: Agent completes the task ──────────────────────────────────
	completeResp := agentReq("POST", fmt.Sprintf("/api/tasks/%s/complete", taskID), nil)
	defer completeResp.Body.Close()
	if completeResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(completeResp.Body)
		t.Fatalf("complete: status %d: %s", completeResp.StatusCode, body)
	}
	t.Log("step 6: task completed — full discovery-to-execution flow passed")
}
