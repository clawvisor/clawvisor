package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/clawvisor/clawvisor/internal/api/middleware"
	"github.com/clawvisor/clawvisor/pkg/adapters"
	"github.com/clawvisor/clawvisor/pkg/store"
)

type generatedCatalogAdapter struct {
	id string
}

func (a *generatedCatalogAdapter) ServiceID() string          { return a.id }
func (a *generatedCatalogAdapter) SupportedActions() []string { return []string{"list_items"} }
func (a *generatedCatalogAdapter) Execute(context.Context, adapters.Request) (*adapters.Result, error) {
	return nil, nil
}
func (a *generatedCatalogAdapter) OAuthConfig() *oauth2.Config                       { return nil }
func (a *generatedCatalogAdapter) CredentialFromToken(*oauth2.Token) ([]byte, error) { return nil, nil }
func (a *generatedCatalogAdapter) ValidateCredential(cred []byte) error {
	if len(cred) == 0 {
		return errors.New("credential required")
	}
	return nil
}
func (a *generatedCatalogAdapter) RequiredScopes() []string { return nil }
func (a *generatedCatalogAdapter) ServiceMetadata() adapters.ServiceMetadata {
	return adapters.ServiceMetadata{
		DisplayName: "Generated API",
		Description: "A user generated API adapter",
		ActionMeta: map[string]adapters.ActionMeta{
			"list_items": {
				DisplayName: "List items",
				Category:    "read",
				Sensitivity: "low",
			},
		},
	}
}

type catalogVault struct {
	keys map[string][]string
}

func (v catalogVault) Set(context.Context, string, string, []byte) error         { return nil }
func (v catalogVault) SetIfAbsent(context.Context, string, string, []byte) error { return nil }
func (v catalogVault) Get(context.Context, string, string) ([]byte, error)       { return nil, nil }
func (v catalogVault) Delete(context.Context, string, string) error              { return nil }
func (v catalogVault) List(_ context.Context, userID string) ([]string, error) {
	return v.keys[userID], nil
}

func TestServicesListIncludesUserGeneratedAdapter(t *testing.T) {
	registry := adapters.NewRegistry()
	registry.SetUserAdapterLister(func(_ context.Context, userID string) ([]adapters.Adapter, bool) {
		if userID != "alice" {
			return nil, true
		}
		return []adapters.Adapter{&generatedCatalogAdapter{id: "generated.api"}}, true
	})

	st := newLocalTestStore()
	st.serviceMetas = []*store.ServiceMeta{{
		UserID:      "alice",
		ServiceID:   "generated.api",
		Alias:       "default",
		ActivatedAt: time.Date(2026, 5, 22, 12, 0, 0, 0, time.UTC),
	}}
	h := NewServicesHandler(st, catalogVault{keys: map[string][]string{
		"alice": {"generated.api"},
	}}, registry, slog.Default(), "http://localhost:9090", nil)

	req := httptest.NewRequest(http.MethodGet, "/api/services", nil)
	req = req.WithContext(context.WithValue(req.Context(), middleware.UserContextKey, &store.User{ID: "alice"}))
	w := httptest.NewRecorder()

	h.List(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Services []struct {
			ID      string `json:"id"`
			Name    string `json:"name"`
			Status  string `json:"status"`
			Actions []struct {
				ID          string `json:"id"`
				DisplayName string `json:"display_name"`
				Category    string `json:"category"`
			} `json:"actions"`
		} `json:"services"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if len(resp.Services) != 1 {
		t.Fatalf("expected one generated service, got %#v", resp.Services)
	}
	svc := resp.Services[0]
	if svc.ID != "generated.api" || svc.Name != "Generated API" || svc.Status != "activated" {
		t.Fatalf("unexpected service entry: %#v", svc)
	}
	if len(svc.Actions) != 1 || svc.Actions[0].ID != "list_items" || svc.Actions[0].Category != "read" {
		t.Fatalf("unexpected action metadata: %#v", svc.Actions)
	}
}
