package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/clawvisor/clawvisor/internal/store/sqlite"
	intvault "github.com/clawvisor/clawvisor/internal/vault"
	"github.com/clawvisor/clawvisor/pkg/store"
)

func TestRuntimeHandlerCreatePlaceholder(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.New(ctx, filepath.Join(t.TempDir(), "runtime-placeholder.db"))
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := sqlite.NewStore(db)
	v, err := intvault.NewLocalVault(filepath.Join(t.TempDir(), "vault.key"), db, "sqlite")
	if err != nil {
		t.Fatalf("NewLocalVault: %v", err)
	}

	user, err := st.CreateUser(ctx, "runtime-placeholder@test.example", "hash")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	agent, err := st.CreateAgent(ctx, user.ID, "runtime-agent", "token-hash")
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	if err := v.Set(ctx, user.ID, "google.gmail:work", []byte(`{"access_token":"real-token"}`)); err != nil {
		t.Fatalf("vault.Set: %v", err)
	}

	body, _ := json.Marshal(map[string]any{"service": "google.gmail:work"})
	req := httptest.NewRequest(http.MethodPost, "/api/runtime/placeholders", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(store.WithAgent(req.Context(), agent))

	rec := httptest.NewRecorder()
	h := NewRuntimeHandler(st, v, nil, nil)
	h.CreatePlaceholder(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("CreatePlaceholder status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	placeholder, _ := resp["placeholder"].(string)
	if placeholder == "" {
		t.Fatal("expected placeholder in response")
	}
	meta, err := st.GetRuntimePlaceholder(ctx, placeholder)
	if err != nil {
		t.Fatalf("GetRuntimePlaceholder: %v", err)
	}
	if meta.AgentID != agent.ID || meta.UserID != user.ID || meta.ServiceID != "google.gmail:work" {
		t.Fatalf("unexpected placeholder metadata: %+v", meta)
	}
}
