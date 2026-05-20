package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
