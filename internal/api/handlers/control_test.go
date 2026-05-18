package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/clawvisor/clawvisor/pkg/store"
	"github.com/clawvisor/clawvisor/pkg/store/sqlite"
)

func TestControlSkillCredentialExampleUsesCurrentVaultItemShape(t *testing.T) {
	h := NewLLMControlHandler("http://localhost:25297")
	req := httptest.NewRequest(http.MethodGet, "/control/skill", nil)
	res := httptest.NewRecorder()

	h.Skill(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("Skill status=%d body=%s", res.Code, res.Body.String())
	}

	body := res.Body.String()
	if got := res.Header().Get("Content-Type"); !strings.Contains(got, "text/markdown") {
		t.Fatalf("expected markdown content type, got %q", got)
	}
	if strings.Contains(res.Body.String(), "vault_github_release_bot") {
		t.Fatalf("skill payload should not contain stale vault item example: %s", res.Body.String())
	}
	for _, want := range []string{
		"# Clawvisor Task Help",
		`"lifetime": "session"`,
		`"expires_in_seconds": 600`,
		`"vault_item_id": "github"`,
		`"why": "Authenticate to GitHub to create the approved issue."`,
		`"lifetime":"standing"`,
		"Never include `expires_in_seconds`",
		"CLAWVISOR_TASK_ID",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("skill markdown missing %q: %s", want, body)
		}
	}
	if strings.Contains(body, `"required_credentials": [{"vault_item_id":"github"}]`) {
		t.Fatalf("skill payload should document standing lifetime constraints: %s", res.Body.String())
	}
}

func TestControlHelpRouterListsFocusedTopics(t *testing.T) {
	h := NewLLMControlHandler("http://localhost:25297")
	req := httptest.NewRequest(http.MethodGet, "/control/help", nil)
	res := httptest.NewRecorder()

	h.Help(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("Help status=%d body=%s", res.Code, res.Body.String())
	}
	if got := res.Header().Get("Content-Type"); !strings.Contains(got, "text/markdown") {
		t.Fatalf("expected markdown content type, got %q", got)
	}
	body := res.Body.String()
	for _, want := range []string{
		"/control/help/tasks",
		"/control/help/credentials",
		"/control/help/tools",
		"/control/help/legacy-adapters",
		"/control/help/errors",
		"/control/help/bug-reporting",
		"/control/tasks?status=active",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("help router missing %q in %s", want, body)
		}
	}
}

func TestControlHelpTopicsAreFocused(t *testing.T) {
	h := NewLLMControlHandler("http://localhost:25297")
	req := httptest.NewRequest(http.MethodGet, "/control/help/bug-reporting", nil)
	req.SetPathValue("topic", "bug-reporting")
	res := httptest.NewRecorder()

	h.HelpTopic(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("HelpTopic status=%d body=%s", res.Code, res.Body.String())
	}
	body := res.Body.String()
	if !strings.Contains(body, "# Clawvisor Bug Reporting Help") || !strings.Contains(body, "request_id") {
		t.Fatalf("bug reporting topic should contain reporting guidance: %s", body)
	}
	if strings.Contains(body, "create_task") || strings.Contains(body, "expected_tools") {
		t.Fatalf("bug reporting topic should not replicate task docs: %s", body)
	}
}

func TestControlHelpToolsUsesAgentContext(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.New(ctx, filepath.Join(t.TempDir(), "control-help-tools.db"))
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := sqlite.NewStore(db)
	user, err := st.CreateUser(ctx, "control-help-tools@example.com", "hash")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	agent, err := st.CreateAgent(ctx, user.ID, "claude-code", "token-hash")
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	if err := st.LogAudit(ctx, &store.AuditEntry{
		ID:        "audit-control-help-tools",
		UserID:    user.ID,
		AgentID:   &agent.ID,
		RequestID: "req-control-help-tools",
		Timestamp: time.Now().UTC(),
		Service:   "anthropic",
		Action:    "lite_proxy.messages.create",
		ParamsSafe: json.RawMessage(`{
			"event":"lite_proxy.endpoint_call",
			"available_tools":["exec_command","Read","Write","Create task"]
		}`),
		Decision: "allow",
		Outcome:  "success",
	}); err != nil {
		t.Fatalf("LogAudit: %v", err)
	}
	if err := st.CreateRuntimePolicyRule(ctx, &store.RuntimePolicyRule{
		ID:       "allow-read",
		UserID:   user.ID,
		AgentID:  &agent.ID,
		Kind:     "tool",
		Action:   "allow",
		ToolName: "Read",
		Source:   "test",
		Enabled:  true,
	}); err != nil {
		t.Fatalf("CreateRuntimePolicyRule allow: %v", err)
	}
	if err := st.CreateRuntimePolicyRule(ctx, &store.RuntimePolicyRule{
		ID:       "deny-write",
		UserID:   user.ID,
		AgentID:  &agent.ID,
		Kind:     "tool",
		Action:   "deny",
		ToolName: "Write",
		Source:   "test",
		Enabled:  true,
	}); err != nil {
		t.Fatalf("CreateRuntimePolicyRule deny: %v", err)
	}

	h := NewLLMControlHandler("http://localhost:25297", st)
	req := httptest.NewRequest(http.MethodGet, "/control/help/tools", nil)
	req.SetPathValue("topic", "tools")
	req = req.WithContext(store.WithAgent(req.Context(), agent))
	res := httptest.NewRecorder()

	h.HelpTopic(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("HelpTopic status=%d body=%s", res.Code, res.Body.String())
	}
	body := res.Body.String()
	for _, want := range []string{
		"Recommended shell tool for control-plane curl: `exec_command`",
		"- `exec_command`",
		"- `Read`",
		"Allowed without a task:\n- `Read`",
		"Always denied:\n- `Write`",
		"Known task-management/meta tools:\n- `Create task`",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("request-aware tools help missing %q:\n%s", want, body)
		}
	}
}

func TestControlFailureIncludesOriginalCommandContext(t *testing.T) {
	h := NewLLMControlHandler("http://localhost:25297")
	body := bytes.NewBufferString(`{"original_tool":"Bash","original_command":"curl -sS 'https://clawvisor.local/control/vault/items' | python3 -c 'print(1)'"}`)
	req := httptest.NewRequest(http.MethodPost, "/control/failure?reason=malformed_control_command", body)
	res := httptest.NewRecorder()

	h.Failure(res, req)

	if res.Code != http.StatusBadRequest {
		t.Fatalf("Failure status=%d body=%s", res.Code, res.Body.String())
	}
	var payload struct {
		Error           string `json:"error"`
		Reason          string `json:"reason"`
		OriginalTool    string `json:"original_tool"`
		OriginalCommand string `json:"original_command"`
		NextStep        string `json:"next_step"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode failure payload: %v", err)
	}
	if payload.Error != "control_command_rejected" || payload.Reason != "malformed_control_command" {
		t.Fatalf("unexpected failure payload: %+v", payload)
	}
	if payload.OriginalTool != "Bash" || !strings.Contains(payload.OriginalCommand, "python3") {
		t.Fatalf("expected original command context, got %+v", payload)
	}
	if !strings.Contains(payload.NextStep, "/control/vault/items") {
		t.Fatalf("expected retry guidance, got %+v", payload)
	}
}
