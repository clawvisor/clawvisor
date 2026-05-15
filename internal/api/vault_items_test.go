package api_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/clawvisor/clawvisor/pkg/store"
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
