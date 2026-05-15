package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/clawvisor/clawvisor/pkg/adapters"
	"github.com/clawvisor/clawvisor/pkg/store"
	"github.com/clawvisor/clawvisor/pkg/store/sqlite"
	intvault "github.com/clawvisor/clawvisor/pkg/vault"
)

func TestVaultControlItemsReturnsCompactAgentList(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.New(ctx, filepath.Join(t.TempDir(), "vault-control.db"))
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := sqlite.NewStore(db)
	v, err := intvault.NewLocalVault(filepath.Join(t.TempDir(), "vault.key"), db, "sqlite")
	if err != nil {
		t.Fatalf("NewLocalVault: %v", err)
	}
	user, err := st.CreateUser(ctx, "vault-control@test.example", "hash")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	agent, err := st.CreateAgent(ctx, user.ID, "agent", "token-hash")
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	if err := v.Set(ctx, user.ID, "agentphone", []byte("secret-value")); err != nil {
		t.Fatalf("Vault.Set: %v", err)
	}

	h := NewVaultHandler(st, v, adapters.NewRegistry())
	req := httptest.NewRequest(http.MethodGet, "/control/vault/items", nil)
	req = req.WithContext(store.WithAgent(req.Context(), agent))
	rec := httptest.NewRecorder()

	h.ListForAgent(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("ListForAgent status=%d body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Items []struct {
			ID       string `json:"id"`
			Label    string `json:"label"`
			Kind     string `json:"kind"`
			Provider string `json:"provider,omitempty"`
		} `json:"items"`
		Entries      []json.RawMessage `json:"entries"`
		Instructions string            `json:"instructions"`
		Total        int               `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(body.Items) != 1 || body.Items[0].ID != "agentphone" {
		t.Fatalf("expected compact item id list, got %+v", body.Items)
	}
	if len(body.Entries) != 0 {
		t.Fatalf("control response should not use dashboard entries shape: %+v", body.Entries)
	}
	if body.Instructions == "" {
		t.Fatal("expected usage instructions")
	}
}

func TestVaultAgentItemsKeepsDashboardShape(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.New(ctx, filepath.Join(t.TempDir(), "vault-agent.db"))
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := sqlite.NewStore(db)
	v, err := intvault.NewLocalVault(filepath.Join(t.TempDir(), "vault.key"), db, "sqlite")
	if err != nil {
		t.Fatalf("NewLocalVault: %v", err)
	}
	user, err := st.CreateUser(ctx, "vault-agent@test.example", "hash")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	agent, err := st.CreateAgent(ctx, user.ID, "agent", "token-hash")
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	if err := v.Set(ctx, user.ID, "manual.secret", []byte("secret-value")); err != nil {
		t.Fatalf("Vault.Set: %v", err)
	}

	h := NewVaultHandler(st, v, adapters.NewRegistry())
	req := httptest.NewRequest(http.MethodGet, "/api/agent/vault/items", nil)
	req = req.WithContext(store.WithAgent(req.Context(), agent))
	rec := httptest.NewRecorder()

	h.ListForAgent(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("ListForAgent status=%d body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Entries []json.RawMessage `json:"entries"`
		Items   []json.RawMessage `json:"items"`
		Total   int               `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(body.Entries) != 1 {
		t.Fatalf("expected dashboard entries shape, got %+v", body)
	}
	if len(body.Items) != 0 {
		t.Fatalf("agent dashboard response should not use compact control items shape: %+v", body.Items)
	}
}
