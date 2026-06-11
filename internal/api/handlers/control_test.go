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

	"github.com/clawvisor/clawvisor/internal/events"
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

func TestControlWaitForApproval(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.New(ctx, filepath.Join(t.TempDir(), "control-wait.db"))
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := sqlite.NewStore(db)

	user, err := st.CreateUser(ctx, "wait-user@test.example", "hash")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	agent, err := st.CreateAgent(ctx, user.ID, "agent", "token-hash")
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}

	h := &LLMControlHandler{
		BaseURL:          "http://localhost:25297",
		Store:            st,
		PendingApprovals: llmproxy.NewMemoryPendingApprovalCache(time.Minute),
	}

	// 1. Task not found / ID required
	{
		req := httptest.NewRequest(http.MethodGet, "/api/control/approvals//wait", nil)
		req = req.WithContext(store.WithAgent(req.Context(), agent))
		res := httptest.NewRecorder()
		h.WaitForApproval(res, req)
		if res.Code != http.StatusBadRequest {
			t.Errorf("expected bad request, got %d", res.Code)
		}
	}

	// 2. Task exists and is approved (active)
	{
		task := &store.Task{
			ID:       "task-active",
			UserID:   user.ID,
			AgentID:  agent.ID,
			Purpose:  "approved work",
			Status:   "active",
			Lifetime: "session",
		}
		if err := st.CreateTask(ctx, task); err != nil {
			t.Fatalf("CreateTask: %v", err)
		}

		req := httptest.NewRequest(http.MethodGet, "/api/control/approvals/task-active/wait", nil)
		req.SetPathValue("id", "task-active")
		req = req.WithContext(store.WithAgent(req.Context(), agent))
		res := httptest.NewRecorder()
		h.WaitForApproval(res, req)
		if res.Code != http.StatusOK {
			t.Errorf("expected OK, got %d: %s", res.Code, res.Body.String())
		}
		var payload map[string]any
		if err := json.Unmarshal(res.Body.Bytes(), &payload); err != nil {
			t.Fatalf("Unmarshal: %v", err)
		}
		if payload["status"] != "approved" {
			t.Errorf("expected status approved, got %v", payload["status"])
		}
	}

	// 3. Task exists and is denied
	{
		task := &store.Task{
			ID:       "task-denied",
			UserID:   user.ID,
			AgentID:  agent.ID,
			Purpose:  "denied work",
			Status:   "denied",
			Lifetime: "session",
		}
		if err := st.CreateTask(ctx, task); err != nil {
			t.Fatalf("CreateTask: %v", err)
		}

		req := httptest.NewRequest(http.MethodGet, "/api/control/approvals/task-denied/wait", nil)
		req.SetPathValue("id", "task-denied")
		req = req.WithContext(store.WithAgent(req.Context(), agent))
		res := httptest.NewRecorder()
		h.WaitForApproval(res, req)
		if res.Code != http.StatusOK {
			t.Errorf("expected OK, got %d", res.Code)
		}
		var payload map[string]any
		if err := json.Unmarshal(res.Body.Bytes(), &payload); err != nil {
			t.Fatalf("Unmarshal: %v", err)
		}
		if payload["status"] != "denied" {
			t.Errorf("expected status denied, got %v", payload["status"])
		}
	}

	// 4. Task pending approval, times out
	{
		task := &store.Task{
			ID:       "task-pending",
			UserID:   user.ID,
			AgentID:  agent.ID,
			Purpose:  "pending work",
			Status:   "pending_approval",
			Lifetime: "session",
		}
		if err := st.CreateTask(ctx, task); err != nil {
			t.Fatalf("CreateTask: %v", err)
		}

		req := httptest.NewRequest(http.MethodGet, "/api/control/approvals/task-pending/wait?timeout=1", nil)
		req.SetPathValue("id", "task-pending")
		req = req.WithContext(store.WithAgent(req.Context(), agent))
		res := httptest.NewRecorder()
		h.WaitForApproval(res, req)
		if res.Code != http.StatusOK {
			t.Errorf("expected OK, got %d", res.Code)
		}
		var payload map[string]any
		if err := json.Unmarshal(res.Body.Bytes(), &payload); err != nil {
			t.Fatalf("Unmarshal: %v", err)
		}
		if payload["status"] != "pending" {
			t.Errorf("expected status pending, got %v", payload["status"])
		}
	}

	// 5. EventHub set: async task approval
	{
		task := &store.Task{
			ID:       "task-async-pending",
			UserID:   user.ID,
			AgentID:  agent.ID,
			Purpose:  "async work",
			Status:   "pending_approval",
			Lifetime: "session",
		}
		if err := st.CreateTask(ctx, task); err != nil {
			t.Fatalf("CreateTask: %v", err)
		}

		hub := events.NewHub()
		h.EventHub = hub
		defer func() { h.EventHub = nil }()

		// Start a goroutine to approve the task after a delay
		go func() {
			time.Sleep(50 * time.Millisecond)
			// Update task in sqlite store
			if err := st.UpdateTaskStatus(ctx, "task-async-pending", "active"); err != nil {
				t.Errorf("failed to update task: %v", err)
			}
			hub.Publish(user.ID, events.Event{Type: "tasks", ID: "task-async-pending"})
		}()

		req := httptest.NewRequest(http.MethodGet, "/api/control/approvals/task-async-pending/wait?timeout=2", nil)
		req.SetPathValue("id", "task-async-pending")
		req = req.WithContext(store.WithAgent(req.Context(), agent))
		res := httptest.NewRecorder()
		h.WaitForApproval(res, req)

		if res.Code != http.StatusOK {
			t.Errorf("expected OK, got %d", res.Code)
		}
		var payload map[string]any
		if err := json.Unmarshal(res.Body.Bytes(), &payload); err != nil {
			t.Fatalf("Unmarshal: %v", err)
		}
		if payload["status"] != "approved" {
			t.Errorf("expected status approved, got %v", payload["status"])
		}
	}
}
