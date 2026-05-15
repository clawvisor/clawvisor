package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestControlSkillCredentialExampleUsesCurrentVaultItemShape(t *testing.T) {
	h := NewLLMControlHandler("http://localhost:25297")
	req := httptest.NewRequest(http.MethodGet, "/control/skill", nil)
	res := httptest.NewRecorder()

	h.Skill(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("Skill status=%d body=%s", res.Code, res.Body.String())
	}

	var payload struct {
		CreateTask struct {
			Body struct {
				RequiredCredentials []struct {
					VaultItemID string `json:"vault_item_id"`
					Why         string `json:"why"`
				} `json:"required_credentials_json"`
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
}
