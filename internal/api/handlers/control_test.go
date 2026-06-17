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

	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy"
	"github.com/clawvisor/clawvisor/pkg/store"
	"github.com/clawvisor/clawvisor/pkg/store/sqlite"
)

func TestControlSkillCredentialExampleUsesCurrentVaultItemShape(t *testing.T) {
	h := NewLLMControlHandler("http://localhost:25297")
	req := httptest.NewRequest(http.MethodGet, "/api/control/skill", nil)
	res := httptest.NewRecorder()

	h.Skill(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("Skill status=%d body=%s", res.Code, res.Body.String())
	}

	var payload struct {
		CreateTask struct {
			Body struct {
				Lifetime            string `json:"lifetime"`
				ExpiresInSeconds    int    `json:"expires_in_seconds"`
				RequiredCredentials []struct {
					VaultItemID string `json:"vault_item_id"`
					Why         string `json:"why"`
				} `json:"required_credentials"`
			} `json:"body"`
		} `json:"create_task"`
		Rules []string `json:"rules"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode skill payload: %v", err)
	}
	if len(payload.CreateTask.Body.RequiredCredentials) != 1 {
		t.Fatalf("expected one credential example, got %+v", payload.CreateTask.Body.RequiredCredentials)
	}
	cred := payload.CreateTask.Body.RequiredCredentials[0]
	if cred.VaultItemID != "google.gmail" {
		t.Fatalf("expected service-scoped vault item example, got %q", cred.VaultItemID)
	}
	if strings.TrimSpace(cred.Why) == "" || strings.Contains(cred.Why, "Describe why") {
		t.Fatalf("expected concrete credential why example, got %q", cred.Why)
	}
	if strings.Contains(res.Body.String(), "vault_github_release_bot") {
		t.Fatalf("skill payload should not contain stale vault item example: %s", res.Body.String())
	}
	if payload.CreateTask.Body.Lifetime != "session" || payload.CreateTask.Body.ExpiresInSeconds != 600 {
		t.Fatalf("expected session lifetime example with expiry, got %+v", payload.CreateTask.Body)
	}
	if !strings.Contains(res.Body.String(), "lifetime=standing") ||
		!strings.Contains(res.Body.String(), "never combine standing with expires_in_seconds") {
		t.Fatalf("skill payload should document standing lifetime constraints: %s", res.Body.String())
	}
}

func TestControlFailureIncludesOriginalCommandContext(t *testing.T) {
	h := NewLLMControlHandler("http://localhost:25297")
	body := bytes.NewBufferString(`{"original_tool":"Bash","original_command":"curl -sS 'https://clawvisor.local/control/vault/items' | python3 -c 'print(1)'"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/control/failure?reason=malformed_control_command", body)
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

func TestControlListTasksReturnsAgentActiveTasksAndCheckout(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.New(ctx, filepath.Join(t.TempDir(), "control-tasks.db"))
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := sqlite.NewStore(db)

	user, err := st.CreateUser(ctx, "control-list@test.example", "hash")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	agent, err := st.CreateAgent(ctx, user.ID, "agent", "token-hash")
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	otherAgent, err := st.CreateAgent(ctx, user.ID, "other-agent", "other-token-hash")
	if err != nil {
		t.Fatalf("CreateAgent(other): %v", err)
	}

	expiresAt := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	for _, task := range []*store.Task{
		{
			ID:            "task-active",
			UserID:        user.ID,
			AgentID:       agent.ID,
			Purpose:       "Send the requested status email",
			Status:        "active",
			Lifetime:      "session",
			ExpiresAt:     &expiresAt,
			ExpectedTools: json.RawMessage(`[{"tool_name":"Bash","why":"Use curl"}]`),
		},
		{
			ID:       "task-other-agent",
			UserID:   user.ID,
			AgentID:  otherAgent.ID,
			Purpose:  "Other agent task",
			Status:   "active",
			Lifetime: "session",
		},
		{
			ID:       "task-pending",
			UserID:   user.ID,
			AgentID:  agent.ID,
			Purpose:  "Pending task",
			Status:   "pending_approval",
			Lifetime: "session",
		},
	} {
		if err := st.CreateTask(ctx, task); err != nil {
			t.Fatalf("CreateTask(%s): %v", task.ID, err)
		}
	}

	checkouts := llmproxy.NewMemoryTaskCheckoutStore(time.Hour)
	if err := checkouts.Set(ctx, llmproxy.TaskCheckoutKey{UserID: user.ID, AgentID: agent.ID}, "task-active", time.Hour); err != nil {
		t.Fatalf("checkout.Set: %v", err)
	}
	h := &LLMControlHandler{
		BaseURL:       "http://localhost:25297",
		Store:         st,
		TaskCheckouts: checkouts,
	}
	req := httptest.NewRequest(http.MethodGet, "/api/control/tasks", nil)
	req = req.WithContext(store.WithAgent(req.Context(), agent))
	res := httptest.NewRecorder()

	h.ListTasks(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("ListTasks status=%d body=%s", res.Code, res.Body.String())
	}
	var payload struct {
		ActiveTaskID string `json:"active_task_id"`
		Total        int    `json:"total"`
		Tasks        []struct {
			ID         string          `json:"id"`
			Purpose    string          `json:"purpose"`
			Status     string          `json:"status"`
			CheckedOut bool            `json:"checked_out"`
			Tools      json.RawMessage `json:"expected_tools"`
		} `json:"tasks"`
		NextStep string `json:"next_step"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if payload.ActiveTaskID != "task-active" || payload.Total != 1 || len(payload.Tasks) != 1 {
		t.Fatalf("unexpected task list payload: %+v", payload)
	}
	got := payload.Tasks[0]
	if got.ID != "task-active" || got.Purpose == "" || got.Status != "active" || !got.CheckedOut {
		t.Fatalf("unexpected task summary: %+v", got)
	}
	if !strings.Contains(string(got.Tools), "Bash") {
		t.Fatalf("expected scope hints in task summary: %+v", got)
	}
	if !strings.Contains(payload.NextStep, "/control/task/checkout") {
		t.Fatalf("expected checkout guidance, got %q", payload.NextStep)
	}
}

// TestControlCapabilitiesAdvertisesCompleteEndpoint locks in the new
// /control/tasks/{id}/complete entry in Capabilities. Without it the
// agent has no discoverable signal that the endpoint exists outside
// the system-prompt notice.
func TestControlCapabilitiesAdvertisesCompleteEndpoint(t *testing.T) {
	h := NewLLMControlHandler("http://localhost:25297")
	req := httptest.NewRequest(http.MethodGet, "/api/control/capabilities", nil)
	res := httptest.NewRecorder()

	h.Capabilities(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("Capabilities status=%d body=%s", res.Code, res.Body.String())
	}
	var payload struct {
		Endpoints []map[string]string `json:"endpoints"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode capabilities: %v", err)
	}
	var found map[string]string
	for _, ep := range payload.Endpoints {
		if ep["path"] == "/control/tasks/{id}/complete" {
			found = ep
			break
		}
	}
	if found == nil {
		t.Fatalf("expected /control/tasks/{id}/complete in endpoints; got %+v", payload.Endpoints)
	}
	if found["method"] != "POST" {
		t.Errorf("complete endpoint method = %q, want POST", found["method"])
	}
	if !strings.Contains(strings.ToLower(found["purpose"]), "complete") {
		t.Errorf("complete endpoint purpose should mention 'complete'; got %q", found["purpose"])
	}
}

// TestControlSkillExposesCompleteTaskBlock locks in the complete_task
// block in the Skill payload. The Skill surface is the per-action
// schema the agent reads when it wants to know "what's the exact
// request shape?".
func TestControlSkillExposesCompleteTaskBlock(t *testing.T) {
	h := NewLLMControlHandler("http://localhost:25297")
	req := httptest.NewRequest(http.MethodGet, "/api/control/skill", nil)
	res := httptest.NewRecorder()

	h.Skill(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("Skill status=%d body=%s", res.Code, res.Body.String())
	}
	var payload struct {
		CompleteTask map[string]any `json:"complete_task"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode skill: %v", err)
	}
	if payload.CompleteTask == nil {
		t.Fatalf("expected complete_task block in skill payload; got %s", res.Body.String())
	}
	if got, _ := payload.CompleteTask["method"].(string); got != "POST" {
		t.Errorf("complete_task method = %q, want POST", got)
	}
	if got, _ := payload.CompleteTask["path"].(string); got != "/control/tasks/{id}/complete" {
		t.Errorf("complete_task path = %q, want /control/tasks/{id}/complete", got)
	}
}
