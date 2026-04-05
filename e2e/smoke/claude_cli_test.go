package smoke_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestClaudeCLISetup launches the real Claude CLI with the /clawvisor-setup
// instructions and verifies it completes the full setup flow against the
// test server. This is a true end-to-end test: a real AI agent follows
// instructions to connect, configure, and smoke-test the Clawvisor gateway.
func TestClaudeCLISetup(t *testing.T) {
	if os.Getenv("CLAWVISOR_E2E_CLAUDE_CLI") == "" {
		t.Skip("skipping: set CLAWVISOR_E2E_CLAUDE_CLI=1 to run Claude CLI tests (uses API credits)")
	}

	claudeBin, err := exec.LookPath("claude")
	if err != nil {
		t.Skip("skipping: claude CLI not found in PATH")
	}
	t.Logf("using claude CLI: %s", claudeBin)

	env := setup(t)

	// Use a temp file for settings output so we don't touch real ~/.claude/settings.json.
	settingsPath := filepath.Join(env.tmpDir, "claude-settings.json")
	if err := os.WriteFile(settingsPath, []byte("{}"), 0644); err != nil {
		t.Fatalf("write initial settings: %v", err)
	}
	t.Logf("settings output path: %s", settingsPath)

	// Build the prompt: the setup command with URLs rewritten to our test server.
	prompt := buildSetupPrompt(t, env.baseURL, settingsPath)

	// Start background approver for connection requests and tasks.
	done := make(chan struct{})
	defer close(done)
	go backgroundApprover(t, env, done)

	// Launch Claude CLI.
	cmd := exec.Command(claudeBin,
		"-p",
		"--dangerously-skip-permissions",
		"--model", "sonnet",
		"--no-session-persistence",
		"--max-budget-usd", "1.00",
		prompt,
	)
	cmd.Dir = env.tmpDir

	// Build env: inherit everything except CLAUDECODE (which blocks nested sessions).
	var cmdEnv []string
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, "CLAUDECODE=") {
			continue
		}
		cmdEnv = append(cmdEnv, e)
	}
	cmdEnv = append(cmdEnv,
		"CLAWVISOR_URL="+env.baseURL,
	)
	cmd.Env = cmdEnv

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	t.Log("launching Claude CLI...")
	startTime := time.Now()
	err = cmd.Run()
	elapsed := time.Since(startTime)
	t.Logf("Claude CLI finished in %s (exit=%v)", elapsed.Round(time.Millisecond), err)

	output := stdout.String()
	if len(output) > 2000 {
		t.Logf("claude output (first 2000 chars):\n%s\n...(truncated)", output[:2000])
	} else {
		t.Logf("claude output:\n%s", output)
	}
	if stderr.Len() > 0 {
		t.Logf("claude stderr:\n%s", stderr.String())
	}

	if err != nil {
		t.Fatalf("claude CLI failed: %v", err)
	}

	// ── Verify the results ────────────────────────────────────────────────

	// 1. Check that Claude wrote the settings file with the token.
	verifySettingsWritten(t, settingsPath)

	// 2. Check that an agent was created via the connection flow.
	verifyAgentConnected(t, env)

	// 3. Look for success indicators in the output.
	verifyOutputIndicators(t, output)
}

// buildSetupPrompt constructs the prompt for Claude CLI, rewriting the
// setup command's hardcoded localhost:25297 to the test server's URL.
func buildSetupPrompt(t *testing.T, baseURL, settingsPath string) string {
	t.Helper()

	return fmt.Sprintf(`You are running the Clawvisor setup flow against a test server.
Follow these steps exactly. This is non-interactive — do not ask for user
confirmation. The server is already running and a background process will
auto-approve all connection requests and tasks within a few seconds.

IMPORTANT: The Clawvisor server is at %s (NOT localhost:25297).
Use %s everywhere instead of http://localhost:25297.

IMPORTANT: Write settings to %s (NOT ~/.claude/settings.json).

## Steps

### 1. Verify the daemon is running

Run: curl -sf %s/ready && echo "RUNNING" || echo "NOT RUNNING"

If RUNNING, proceed. If not, stop and report failure.

### 2. Connect as an agent

Run: curl -s -X POST "%s/api/agents/connect?wait=true&timeout=30" -H "Content-Type: application/json" -d '{"name": "claude-code", "description": "Claude Code agent"}'

Parse the JSON response. Extract the "token" field. If status is not "approved",
wait a moment and retry — a background process will approve within seconds.

### 3. Set environment variables

Read %s (it already exists with "{}").
Write the following JSON to it:
{
  "env": {
    "CLAWVISOR_URL": "%s",
    "CLAWVISOR_AGENT_TOKEN": "<token from step 2>"
  }
}

### 4. Verify

Run: curl -sf -H "Authorization: Bearer <token>" %s/api/skill/catalog | head -20

Confirm it returns a service catalog. If it returns 401, something went wrong.

### 5. Smoke test

Pick any service from the catalog that is activated. Create a read-only task:

curl -s -X POST "%s/api/tasks?wait=true&timeout=30" -H "Authorization: Bearer <token>" -H "Content-Type: application/json" -d '{"purpose": "Setup smoke test", "authorized_actions": [{"service": "<svc>", "action": "<read_action>", "auto_execute": true}]}'

Then make an in-scope gateway request:

curl -s -X POST "%s/api/gateway/request?wait=true&timeout=30" -H "Authorization: Bearer <token>" -H "Content-Type: application/json" -d '{"service": "<svc>", "action": "<read_action>", "params": {}, "reason": "Setup smoke test", "request_id": "e2e-cli-test", "task_id": "<task_id>"}'

Report whether the request succeeded (status=executed) or was restricted.

Then complete the task:

curl -s -X POST "%s/api/tasks/<task_id>/complete" -H "Authorization: Bearer <token>"

### 6. Done

Print "CLAWVISOR_SETUP_COMPLETE" to indicate success. Do NOT offer to uninstall
anything. Summarize what happened.`,
		baseURL, baseURL,
		settingsPath,
		baseURL,
		baseURL,
		settingsPath, baseURL,
		baseURL,
		baseURL,
		baseURL,
		baseURL,
	)
}

// backgroundApprover continuously approves any pending connection requests
// and tasks while the Claude CLI is running.
func backgroundApprover(t *testing.T, env *e2eEnv, done <-chan struct{}) {
	t.Helper()
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			// Approve pending connections.
			connResp := env.userDo("GET", "/api/agents/connections", nil)
			connBody, _ := io.ReadAll(connResp.Body)
			connResp.Body.Close()
			if connResp.StatusCode == http.StatusOK {
				var connections []map[string]any
				if json.Unmarshal(connBody, &connections) == nil {
					for _, c := range connections {
						if strOr(c, "status", "") == "pending" {
							id := strOr(c, "id", "")
							if id != "" {
								r := env.userDo("POST", fmt.Sprintf("/api/agents/connect/%s/approve", id), nil)
								r.Body.Close()
								if r.StatusCode == http.StatusOK {
									t.Logf("[approver] approved connection %s", id)
								}
							}
						}
					}
				}
			}

			// Approve pending tasks.
			taskResp := env.userDo("GET", "/api/tasks", nil)
			taskBody, _ := io.ReadAll(taskResp.Body)
			taskResp.Body.Close()
			if taskResp.StatusCode == http.StatusOK {
				var m map[string]any
				if json.Unmarshal(taskBody, &m) == nil {
					tasks, _ := m["tasks"].([]any)
					for _, raw := range tasks {
						task, ok := raw.(map[string]any)
						if !ok {
							continue
						}
						status := strOr(task, "status", "")
						id := strOr(task, "id", "")
						if id != "" && (status == "pending_approval" || status == "pending") {
							r := env.userDo("POST", fmt.Sprintf("/api/tasks/%s/approve", id), nil)
							r.Body.Close()
							if r.StatusCode == http.StatusOK {
								t.Logf("[approver] approved task %s", id)
							}
						}
						if id != "" && status == "pending_scope_expansion" {
							r := env.userDo("POST", fmt.Sprintf("/api/tasks/%s/expand/approve", id), nil)
							r.Body.Close()
							if r.StatusCode == http.StatusOK {
								t.Logf("[approver] approved expansion for task %s", id)
							}
						}
					}
				}
			}

			// Approve pending gateway requests.
			approvalsResp := env.userDo("GET", "/api/approvals", nil)
			approvalsBody, _ := io.ReadAll(approvalsResp.Body)
			approvalsResp.Body.Close()
			if approvalsResp.StatusCode == http.StatusOK {
				var approvals []map[string]any
				if json.Unmarshal(approvalsBody, &approvals) == nil {
					for _, a := range approvals {
						if strOr(a, "status", "") == "pending" {
							reqID := strOr(a, "request_id", "")
							if reqID != "" {
								r := env.userDo("POST", fmt.Sprintf("/api/approvals/%s/approve", reqID), nil)
								r.Body.Close()
								if r.StatusCode == http.StatusOK {
									t.Logf("[approver] approved request %s", reqID)
								}
							}
						}
					}
				}
			}
		}
	}
}

// verifySettingsWritten checks that Claude wrote CLAWVISOR_AGENT_TOKEN
// into the settings file.
func verifySettingsWritten(t *testing.T, settingsPath string) {
	t.Helper()
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Errorf("settings.json not readable: %v", err)
		return
	}

	var settings map[string]any
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Errorf("settings.json not valid JSON: %v", err)
		return
	}

	envMap, ok := settings["env"].(map[string]any)
	if !ok {
		t.Error("settings.json missing 'env' object")
		return
	}

	token, ok := envMap["CLAWVISOR_AGENT_TOKEN"].(string)
	if !ok || token == "" {
		t.Error("settings.json missing CLAWVISOR_AGENT_TOKEN")
		return
	}
	if !strings.HasPrefix(token, "cvis_") {
		t.Errorf("token doesn't have cvis_ prefix: %s", token[:min(len(token), 10)])
		return
	}
	t.Logf("verified: settings.json has CLAWVISOR_AGENT_TOKEN (%d chars)", len(token))

	url, _ := envMap["CLAWVISOR_URL"].(string)
	if url == "" {
		t.Error("settings.json missing CLAWVISOR_URL")
	} else {
		t.Logf("verified: settings.json has CLAWVISOR_URL=%s", url)
	}
}

// verifyAgentConnected checks that a "claude-code" agent was registered.
func verifyAgentConnected(t *testing.T, env *e2eEnv) {
	t.Helper()
	resp := env.userDo("GET", "/api/agents", nil)
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("list agents: %d", resp.StatusCode)
		return
	}

	var agents []map[string]any
	if err := json.Unmarshal(b, &agents); err != nil {
		t.Errorf("parse agents: %v", err)
		return
	}

	for _, a := range agents {
		name := strOr(a, "name", "")
		if name == "claude-code" {
			t.Logf("verified: claude-code agent exists (id=%s)", strOr(a, "id", "?"))
			return
		}
	}
	t.Error("claude-code agent not found in agents list")
}

// verifyOutputIndicators checks Claude's output for signs the setup succeeded.
func verifyOutputIndicators(t *testing.T, output string) {
	t.Helper()
	lower := strings.ToLower(output)

	// Check for the explicit success marker we asked Claude to print.
	if strings.Contains(output, "CLAWVISOR_SETUP_COMPLETE") {
		t.Log("verified: Claude printed CLAWVISOR_SETUP_COMPLETE")
	} else {
		t.Error("Claude did not print CLAWVISOR_SETUP_COMPLETE")
	}

	// Check for signs of successful steps.
	indicators := []struct {
		name   string
		marker string
	}{
		{"daemon running", "running"},
		{"token received", "cvis_"},
		{"catalog fetched", "catalog"},
	}
	for _, ind := range indicators {
		if strings.Contains(lower, ind.marker) {
			t.Logf("verified: output mentions %s", ind.name)
		}
	}

	// Check for error indicators.
	errorMarkers := []string{"not running", "401", "failed to connect", "error"}
	for _, em := range errorMarkers {
		if strings.Contains(lower, em) {
			// Not necessarily a failure — Claude might be reporting what happened.
			t.Logf("note: output contains %q (may be informational)", em)
		}
	}
}
