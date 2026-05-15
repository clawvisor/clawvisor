package api_test

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/clawvisor/clawvisor/pkg/adapters"
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

func TestVaultItemsSplitSharedSecretServiceBindings(t *testing.T) {
	env := newTestEnv(t,
		newSharedVaultMockAdapter("mock.mail", "mock.shared", "read"),
		newSharedVaultMockAdapter("mock.calendar", "mock.shared", "read"),
	)
	sc := newScenario(t, env, "vault-items-shared")

	if err := env.Vault.Set(context.Background(), sc.session.UserID, "mock.shared", []byte(`{"type":"api_key","token":"test-token"}`)); err != nil {
		t.Fatalf("Vault.Set: %v", err)
	}
	if err := env.Store.UpsertServiceMeta(context.Background(), sc.session.UserID, "mock.mail", "default", time.Now().UTC()); err != nil {
		t.Fatalf("UpsertServiceMeta mail: %v", err)
	}
	if err := env.Store.UpsertServiceMeta(context.Background(), sc.session.UserID, "mock.calendar", "default", time.Now().UTC()); err != nil {
		t.Fatalf("UpsertServiceMeta calendar: %v", err)
	}
	if err := env.Store.CreateRuntimePlaceholder(context.Background(), &store.RuntimePlaceholder{
		Placeholder: "cv_mock_mail_placeholder",
		UserID:      sc.session.UserID,
		AgentID:     sc.AgentID,
		ServiceID:   "mock.mail",
	}); err != nil {
		t.Fatalf("CreateRuntimePlaceholder: %v", err)
	}

	resp := sc.session.do("GET", "/api/vault/items", nil)
	body := mustStatus(t, resp, http.StatusOK)
	items := arr(t, body, "entries")
	if len(items) != 2 {
		t.Fatalf("expected one row per shared-secret service binding, got %v", body["entries"])
	}
	counts := map[string]float64{}
	for _, raw := range items {
		item := raw.(map[string]any)
		counts[item["id"].(string)] = item["active_placeholder_count"].(float64)
	}
	if counts["mock.mail"] != 1 || counts["mock.calendar"] != 0 {
		t.Fatalf("shared-secret service rows should have separate placeholder counts, got %v", counts)
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

type sharedVaultMockAdapter struct {
	*mockAdapter
	vaultKey string
}

func newSharedVaultMockAdapter(serviceID, vaultKey string, actions ...string) *sharedVaultMockAdapter {
	return &sharedVaultMockAdapter{mockAdapter: newMockAdapter(serviceID, actions...), vaultKey: vaultKey}
}

func (m *sharedVaultMockAdapter) ServiceMetadata() adapters.ServiceMetadata {
	return adapters.ServiceMetadata{
		DisplayName: m.serviceID,
		VaultKey:    m.vaultKey,
	}
}
