package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
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
		Items        []string          `json:"items"`
		Entries      []json.RawMessage `json:"entries"`
		Instructions string            `json:"instructions"`
		Total        int               `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(body.Items) != 1 || body.Items[0] != "agentphone" {
		t.Fatalf("expected compact item id list, got %+v", body.Items)
	}
	if len(body.Entries) != 0 {
		t.Fatalf("control response should not use dashboard entries shape: %+v", body.Entries)
	}
	if body.Instructions == "" {
		t.Fatal("expected usage instructions")
	}
	if !strings.Contains(body.Instructions, "required_credentials_json[].why") {
		t.Fatalf("expected instructions to mention credential why, got %q", body.Instructions)
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

func TestVaultControlItemDetailReturnsCompactMetadata(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.New(ctx, filepath.Join(t.TempDir(), "vault-control-detail.db"))
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := sqlite.NewStore(db)
	v, err := intvault.NewLocalVault(filepath.Join(t.TempDir(), "vault.key"), db, "sqlite")
	if err != nil {
		t.Fatalf("NewLocalVault: %v", err)
	}
	user, err := st.CreateUser(ctx, "vault-control-detail@test.example", "hash")
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
	req := httptest.NewRequest(http.MethodGet, "/control/vault/items/agentphone", nil)
	req.SetPathValue("id", "agentphone")
	req = req.WithContext(store.WithAgent(req.Context(), agent))
	rec := httptest.NewRecorder()

	h.GetForAgent(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GetForAgent status=%d body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["id"] != "agentphone" || body["kind"] != "secret" {
		t.Fatalf("unexpected compact detail payload: %+v", body)
	}
	if instructions, _ := body["instructions"].(string); !strings.Contains(instructions, "required_credentials_json[].why") {
		t.Fatalf("expected detail instructions to mention credential why, got %q", instructions)
	}
	if _, ok := body["secret"]; ok {
		t.Fatalf("control detail must not expose secret material: %+v", body)
	}
	if _, ok := body["placeholders"]; ok {
		t.Fatalf("control detail should not return dashboard placeholder history: %+v", body)
	}
	if _, ok := body["service_bindings"]; ok {
		t.Fatalf("empty detail slices should be omitted: %+v", body)
	}
}
