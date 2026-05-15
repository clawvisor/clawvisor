package api_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"
)

func TestTaskCreateV2EnvelopeOnly(t *testing.T) {
	env := newTestEnv(t)
	sc := newScenario(t, env, "task-v2")

	resp := env.do("POST", "/api/tasks", sc.AgentToken, map[string]any{
		"purpose": "review release issues and fetch matching repository metadata",
		"expected_tools_json": []map[string]any{
			{
				"tool_name": "github.search_issues",
				"why":       "Search for open release-blocking issues in the main repository.",
			},
		},
		"expected_egress_json": []map[string]any{
			{
				"host":   "api.github.com",
				"method": "GET",
				"path":   "/search/issues",
				"why":    "Read the matching issue metadata from GitHub.",
			},
		},
		"intent_verification_mode": "strict",
		"expected_use":             "Review issue metadata for the current release candidate.",
	})
	body := mustStatus(t, resp, http.StatusCreated)
	taskID := str(t, body, "task_id")

	resp = env.do("GET", fmt.Sprintf("/api/tasks/%s", taskID), sc.AgentToken, nil)
	task := mustStatus(t, resp, http.StatusOK)

	if task["schema_version"] != float64(2) {
		t.Fatalf("expected schema_version=2, got %v", task["schema_version"])
	}
	if task["intent_verification_mode"] != "strict" {
		t.Fatalf("expected strict intent verification mode, got %v", task["intent_verification_mode"])
	}
	if task["expected_use"] != "Review issue metadata for the current release candidate." {
		t.Fatalf("unexpected expected_use: %v", task["expected_use"])
	}
	if len(arr(t, task, "expected_tools_json")) != 1 {
		t.Fatalf("expected one expected tool, got %v", task["expected_tools_json"])
	}
	if len(arr(t, task, "expected_egress_json")) != 1 {
		t.Fatalf("expected one expected egress item, got %v", task["expected_egress_json"])
	}
	if task["risk_level"] == nil || task["risk_level"] == "" {
		t.Fatal("expected risk_level to be populated for v2 envelope task")
	}
}

func TestTaskCreateV2RejectsInvalidEnvelope(t *testing.T) {
	env := newTestEnv(t)
	sc := newScenario(t, env, "task-v2-invalid")

	resp := env.do("POST", "/api/tasks", sc.AgentToken, map[string]any{
		"purpose": "inspect runtime calls",
		"expected_tools_json": []map[string]any{
			{
				"tool_name":   "github.search_issues",
				"why":         "short",
				"input_regex": "(",
			},
		},
		"intent_verification_mode": "unsafe",
	})
	body := mustStatus(t, resp, http.StatusBadRequest)

	if body["code"] != "INVALID_REQUEST" {
		t.Fatalf("expected INVALID_REQUEST, got %v", body["code"])
	}
	if body["error"] == nil {
		t.Fatal("expected detailed validation error")
	}
}

func TestTaskCreateV2StoresRequiredCredentialsAndRisk(t *testing.T) {
	env := newTestEnv(t, newMockAdapter("mock.release", "write"))
	sc := newScenario(t, env, "task-v2-credentials")
	sc.activateService(t, env, "mock.release")

	resp := env.do("POST", "/api/tasks", sc.AgentToken, map[string]any{
		"purpose": "create release issues in GitHub",
		"expected_tools_json": []map[string]any{
			{
				"tool_name": "Bash",
				"why":       "Call the GitHub API to create release issues.",
			},
		},
		"required_credentials_json": []map[string]any{
			{
				"vault_item_id": "mock.release",
				"why":           "Use the release credential to create issues in owner/repo.",
			},
		},
	})
	body := mustStatus(t, resp, http.StatusCreated)
	taskID := str(t, body, "task_id")

	resp = env.do("GET", fmt.Sprintf("/api/tasks/%s", taskID), sc.AgentToken, nil)
	task := mustStatus(t, resp, http.StatusOK)
	required := arr(t, task, "required_credentials_json")
	if len(required) != 1 {
		t.Fatalf("expected one required credential, got %v", task["required_credentials_json"])
	}
	if task["risk_level"] != "medium" {
		t.Fatalf("expected medium risk for credential request, got %v", task["risk_level"])
	}

	resp = sc.session.do("POST", fmt.Sprintf("/api/tasks/%s/approve", taskID), nil)
	approved := mustStatus(t, resp, http.StatusOK)
	placeholders := arr(t, approved, "credential_placeholders")
	if len(placeholders) != 1 {
		t.Fatalf("expected one minted credential placeholder, got %v", approved["credential_placeholders"])
	}
	placeholder := placeholders[0].(map[string]any)
	if placeholder["vault_item_id"] != "mock.release" || placeholder["task_id"] != taskID {
		t.Fatalf("unexpected placeholder metadata: %v", placeholder)
	}
}

func TestTaskCreateV2AcceptsVirtualLLMCredentialItem(t *testing.T) {
	env := newTestEnv(t)
	sc := newScenario(t, env, "task-v2-llm-credential")
	storageKey := "agent:" + sc.AgentID + ":anthropic"
	virtualID := "llm:anthropic:agent:" + sc.AgentID
	if err := env.Vault.Set(context.Background(), sc.session.UserID, storageKey, []byte("sk-ant-test-key")); err != nil {
		t.Fatalf("Vault.Set: %v", err)
	}

	resp := env.do("POST", "/api/tasks", sc.AgentToken, map[string]any{
		"purpose": "call Anthropic with an agent-scoped key",
		"expected_tools_json": []map[string]any{{
			"tool_name": "Bash",
			"why":       "Run a curl request to api.anthropic.com for this task.",
		}},
		"required_credentials_json": []map[string]any{{
			"vault_item_id": virtualID,
			"why":           "Use this agent-scoped Anthropic key only for the approved request.",
		}},
	})
	body := mustStatus(t, resp, http.StatusCreated)
	taskID := str(t, body, "task_id")

	resp = sc.session.do("POST", fmt.Sprintf("/api/tasks/%s/approve", taskID), nil)
	approved := mustStatus(t, resp, http.StatusOK)
	placeholders := arr(t, approved, "credential_placeholders")
	if len(placeholders) != 1 {
		t.Fatalf("expected one minted credential placeholder, got %v", approved["credential_placeholders"])
	}
	placeholder := placeholders[0].(map[string]any)
	if placeholder["vault_item_id"] != virtualID || placeholder["service_id"] != virtualID {
		t.Fatalf("unexpected placeholder metadata: %v", placeholder)
	}
}

func TestTaskCreateV2SharedCredentialItemKeepsServiceScope(t *testing.T) {
	env := newTestEnv(t,
		newSharedVaultMockAdapter("mock.mail", "mock.shared", "read"),
		newSharedVaultMockAdapter("mock.calendar", "mock.shared", "read"),
	)
	sc := newScenario(t, env, "task-v2-shared-credential")
	if err := env.Vault.Set(context.Background(), sc.session.UserID, "mock.shared", []byte(`{"type":"api_key","token":"test-token"}`)); err != nil {
		t.Fatalf("Vault.Set: %v", err)
	}

	resp := env.do("POST", "/api/tasks", sc.AgentToken, map[string]any{
		"purpose": "read mail",
		"expected_tools_json": []map[string]any{{
			"tool_name": "mock.mail.read",
			"why":       "Read mail for this task.",
		}},
		"required_credentials_json": []map[string]any{{
			"vault_item_id": "mock.mail",
			"why":           "Use the mail credential only for mail.",
		}},
	})
	body := mustStatus(t, resp, http.StatusCreated)
	taskID := str(t, body, "task_id")

	resp = sc.session.do("POST", fmt.Sprintf("/api/tasks/%s/approve", taskID), nil)
	approved := mustStatus(t, resp, http.StatusOK)
	placeholders := arr(t, approved, "credential_placeholders")
	if len(placeholders) != 1 {
		t.Fatalf("expected one minted credential placeholder, got %v", approved["credential_placeholders"])
	}
	placeholder := placeholders[0].(map[string]any)
	if placeholder["vault_item_id"] != "mock.mail" || placeholder["service_id"] != "mock.mail" {
		t.Fatalf("shared backing secret should preserve service-scoped placeholder identity: %v", placeholder)
	}
}

func TestTaskCreateRejectsPlannedCallsWithoutAuthorizedActions(t *testing.T) {
	env := newTestEnv(t)
	sc := newScenario(t, env, "task-v2-planned")

	resp := env.do("POST", "/api/tasks", sc.AgentToken, map[string]any{
		"purpose": "inspect runtime calls",
		"expected_tools_json": []map[string]any{
			{
				"tool_name": "github.search_issues",
				"why":       "Search for release issues in the repository.",
			},
		},
		"planned_calls": []map[string]any{
			{
				"service": "github",
				"action":  "list_issues",
				"reason":  "Read the current open issues.",
			},
		},
	})
	body := mustStatus(t, resp, http.StatusBadRequest)

	if body["code"] != "INVALID_REQUEST" {
		t.Fatalf("expected INVALID_REQUEST, got %v", body["code"])
	}
}

func TestTaskCreateAcceptsMixedLegacyAndV2Scope(t *testing.T) {
	env := newTestEnv(t, newMockAdapter("mock.task-mixed", "read"))
	sc := newScenario(t, env, "task-mixed")
	sc.activateService(t, env, "mock.task-mixed")

	resp := env.do("POST", "/api/tasks", sc.AgentToken, map[string]any{
		"purpose": "review account state and matching runtime requests",
		"authorized_actions": []map[string]any{{
			"service": "mock.task-mixed", "action": "read", "auto_execute": true,
		}},
		"expected_egress_json": []map[string]any{
			{
				"host":   "api.example.com",
				"method": "GET",
				"path":   "/v1/accounts",
				"why":    "Read account state from the downstream runtime API.",
			},
		},
		"schema_version": 2,
	})
	body := mustStatus(t, resp, http.StatusCreated)
	taskID := str(t, body, "task_id")

	resp = env.do("GET", fmt.Sprintf("/api/tasks/%s", taskID), sc.AgentToken, nil)
	task := mustStatus(t, resp, http.StatusOK)
	if len(arr(t, task, "authorized_actions")) != 1 {
		t.Fatalf("expected legacy authorized action to persist, got %v", task["authorized_actions"])
	}
	if len(arr(t, task, "expected_egress_json")) != 1 {
		t.Fatalf("expected v2 egress item to persist, got %v", task["expected_egress_json"])
	}
}
