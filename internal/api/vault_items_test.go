package api_test

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/clawvisor/clawvisor/pkg/store"
	"github.com/clawvisor/clawvisor/pkg/vault"
)

func TestVaultItemsListForUserAndAgent(t *testing.T) {
	env := newTestEnv(t, newMockAdapter("mock.vault", "read"))
	sc := newScenario(t, env, "vault-items")
	sc.activateService(t, env, "mock.vault")

	usedAt := time.Now().UTC().Truncate(time.Second)
	if err := env.Store.CreateRuntimePlaceholder(context.Background(), &store.RuntimePlaceholder{
		Placeholder: "cv_mock_vault_placeholder",
		UserID:      sc.session.UserID,
		AgentID:     sc.AgentID,
		ServiceID:   "mock.vault",
		LastUsedAt:  &usedAt,
	}); err != nil {
		t.Fatalf("CreateRuntimePlaceholder: %v", err)
	}

	resp := sc.session.do("GET", "/api/vault/items", nil)
	body := mustStatus(t, resp, http.StatusOK)
	items := arr(t, body, "entries")
	if len(items) != 1 {
		t.Fatalf("expected one vault item, got %v", body["entries"])
	}
	item := items[0].(map[string]any)
	if item["id"] != "mock.vault" {
		t.Fatalf("unexpected vault item id %v", item["id"])
	}
	if item["kind"] != "connected_account" {
		t.Fatalf("unexpected vault item kind %v", item["kind"])
	}
	if item["active_placeholder_count"] != float64(1) {
		t.Fatalf("unexpected active_placeholder_count %v", item["active_placeholder_count"])
	}
	if _, ok := item["secret"]; ok {
		t.Fatal("vault item response must not expose secret material")
	}

	resp = env.do("GET", "/api/agent/vault/items", sc.AgentToken, nil)
	agentBody := mustStatus(t, resp, http.StatusOK)
	if len(arr(t, agentBody, "entries")) != 1 {
		t.Fatalf("agent credential discovery should return all vault item labels, got %v", agentBody["entries"])
	}

	resp = sc.session.do("GET", "/api/vault/items/mock.vault", nil)
	detail := mustStatus(t, resp, http.StatusOK)
	if detail["id"] != "mock.vault" {
		t.Fatalf("unexpected detail id %v", detail["id"])
	}
	if len(arr(t, detail, "placeholders")) != 1 {
		t.Fatalf("expected detail placeholder history, got %v", detail["placeholders"])
	}
}

func TestVaultItemsCountPlaceholdersForServiceBindingAliases(t *testing.T) {
	env := newTestEnv(t, newMockAdapter("mock.vault", "read"))
	sc := newScenario(t, env, "vault-items-alias")
	usedAt := time.Now().UTC().Truncate(time.Second)

	if err := env.Vault.Set(context.Background(), sc.session.UserID, "mock.vault:work", []byte(`{"type":"api_key","token":"test-token"}`)); err != nil {
		t.Fatalf("Vault.Set: %v", err)
	}
	if err := env.Store.UpsertServiceMeta(context.Background(), sc.session.UserID, "mock.vault", "work", time.Now().UTC()); err != nil {
		t.Fatalf("UpsertServiceMeta: %v", err)
	}
	if err := env.Store.CreateRuntimePlaceholder(context.Background(), &store.RuntimePlaceholder{
		Placeholder: "cv_mock_vault_work_placeholder",
		UserID:      sc.session.UserID,
		AgentID:     sc.AgentID,
		ServiceID:   "mock.vault:work",
		LastUsedAt:  &usedAt,
	}); err != nil {
		t.Fatalf("CreateRuntimePlaceholder: %v", err)
	}

	resp := sc.session.do("GET", "/api/vault/items", nil)
	body := mustStatus(t, resp, http.StatusOK)
	items := arr(t, body, "entries")
	if len(items) != 1 {
		t.Fatalf("expected one vault item, got %v", body["entries"])
	}
	item := items[0].(map[string]any)
	if item["id"] != "mock.vault:work" || item["active_placeholder_count"] != float64(1) {
		t.Fatalf("unexpected alias vault item stats: %v", item)
	}

	resp = sc.session.do("GET", "/api/vault/items/mock.vault:work", nil)
	detail := mustStatus(t, resp, http.StatusOK)
	if len(arr(t, detail, "placeholders")) != 1 {
		t.Fatalf("expected alias-bound placeholder history, got %v", detail["placeholders"])
	}
}

func TestVaultItemsOmitAgentScopedLLMCredentials(t *testing.T) {
	env := newTestEnv(t)
	sc := newScenario(t, env, "vault-llm-agent")

	serviceID := "agent:" + sc.AgentID + ":anthropic"
	if err := env.Vault.Set(context.Background(), sc.session.UserID, serviceID, []byte("sk-ant-test-key")); err != nil {
		t.Fatalf("Vault.Set: %v", err)
	}
	if err := env.Store.UpsertServiceMeta(context.Background(), sc.session.UserID, serviceID, "default", time.Now().UTC()); err != nil {
		t.Fatalf("UpsertServiceMeta: %v", err)
	}

	resp := sc.session.do("GET", "/api/vault/items", nil)
	body := mustStatus(t, resp, http.StatusOK)
	items := arr(t, body, "entries")
	if len(items) != 0 {
		t.Fatalf("agent-scoped llm credentials should be omitted from vault inventory, got %v", body["entries"])
	}
}

func TestVaultItemsUpdateAndDeleteSecret(t *testing.T) {
	env := newTestEnv(t)
	sc := newScenario(t, env, "vault-secret-edit")

	if err := env.Vault.Set(context.Background(), sc.session.UserID, "manual.secret", []byte("old-value")); err != nil {
		t.Fatalf("Vault.Set: %v", err)
	}

	resp := sc.session.do("PUT", "/api/vault/items/manual.secret", map[string]any{"value": "new-value"})
	mustStatus(t, resp, http.StatusOK)
	got, err := env.Vault.Get(context.Background(), sc.session.UserID, "manual.secret")
	if err != nil {
		t.Fatalf("Vault.Get: %v", err)
	}
	if string(got) != "new-value" {
		t.Fatalf("unexpected updated secret value %q", string(got))
	}

	resp = sc.session.do("DELETE", "/api/vault/items/manual.secret", nil)
	mustStatus(t, resp, http.StatusOK)
	_, err = env.Vault.Get(context.Background(), sc.session.UserID, "manual.secret")
	if !errors.Is(err, vault.ErrNotFound) {
		t.Fatalf("expected deleted vault item, got %v", err)
	}
}
