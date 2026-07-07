package handlers

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/clawvisor/clawvisor/internal/api/middleware"
	"github.com/clawvisor/clawvisor/pkg/adapters"
	"github.com/clawvisor/clawvisor/pkg/store"
	sqlitestore "github.com/clawvisor/clawvisor/pkg/store/sqlite"
)

// newServiceConfigTestHandler builds a ServicesHandler backed by a temp SQLite
// store and a single user, returning the handler and the user.
func newServiceConfigTestHandler(t *testing.T) (*ServicesHandler, *store.User) {
	t.Helper()
	ctx := context.Background()
	db, err := sqlitestore.New(ctx, t.TempDir()+"/test.db")
	if err != nil {
		t.Fatalf("sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := sqlitestore.NewStore(db)
	user, err := st.CreateUser(ctx, "config@test.example", "hash")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	reg := adapters.NewRegistry()
	reg.Register(resolverOAuthTestAdapter{serviceID: "svc"})
	h := NewServicesHandler(st, nil, reg,
		slog.New(slog.NewTextHandler(discardWriter{}, nil)), "", nil)
	return h, user
}

func configRequest(user *store.User, method, target, serviceID, body string) *http.Request {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
	}
	r.SetPathValue("serviceID", serviceID)
	ctx := context.WithValue(r.Context(), middleware.UserContextKey, user)
	return r.WithContext(ctx)
}

// TestServiceConfigCRUD exercises the config document lifecycle the Terraform
// provider's clawvisor_service_config resource drives: upsert, read back,
// overwrite, delete, then 404 on a subsequent read.
func TestServiceConfigCRUD(t *testing.T) {
	h, user := newServiceConfigTestHandler(t)

	// Not found before any write.
	rec := httptest.NewRecorder()
	h.GetConfig(rec, configRequest(user, http.MethodGet, "/api/services/svc/config", "svc", ""))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET before write: got %d, want 404", rec.Code)
	}

	// Upsert.
	rec = httptest.NewRecorder()
	h.PutConfig(rec, configRequest(user, http.MethodPut, "/api/services/svc/config", "svc",
		`{"alias":"default","config":{"region":"us-east-1","retries":3}}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT: got %d (%s), want 200", rec.Code, rec.Body.String())
	}

	// Read back.
	rec = httptest.NewRecorder()
	h.GetConfig(rec, configRequest(user, http.MethodGet, "/api/services/svc/config?alias=default", "svc", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET after write: got %d, want 200", rec.Code)
	}
	var got serviceConfigResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ServiceID != "svc" || got.Alias != "default" {
		t.Fatalf("unexpected response: %+v", got)
	}
	if !json.Valid(got.Config) || !strings.Contains(string(got.Config), "us-east-1") {
		t.Fatalf("config not round-tripped: %s", got.Config)
	}

	// Overwrite.
	rec = httptest.NewRecorder()
	h.PutConfig(rec, configRequest(user, http.MethodPut, "/api/services/svc/config", "svc",
		`{"config":{"region":"eu-west-1"}}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT overwrite: got %d, want 200", rec.Code)
	}

	// Delete.
	rec = httptest.NewRecorder()
	h.DeleteConfig(rec, configRequest(user, http.MethodDelete, "/api/services/svc/config?alias=default", "svc", ""))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE: got %d, want 204", rec.Code)
	}

	// Gone.
	rec = httptest.NewRecorder()
	h.GetConfig(rec, configRequest(user, http.MethodGet, "/api/services/svc/config", "svc", ""))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET after delete: got %d, want 404", rec.Code)
	}
}

// TestServiceConfigPutValidation rejects malformed input the provider could
// never send but a raw client might.
func TestServiceConfigPutValidation(t *testing.T) {
	h, user := newServiceConfigTestHandler(t)

	// Missing config body.
	rec := httptest.NewRecorder()
	h.PutConfig(rec, configRequest(user, http.MethodPut, "/api/services/svc/config", "svc", `{"alias":"default"}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing config: got %d, want 400", rec.Code)
	}

	// Invalid JSON config value.
	rec = httptest.NewRecorder()
	h.PutConfig(rec, configRequest(user, http.MethodPut, "/api/services/svc/config", "svc", `{"config":"not-an-object"`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid JSON: got %d, want 400", rec.Code)
	}
}

// TestServiceConfigPutUnknownService rejects a typo'd/unsupported service ID so
// no orphan config row is persisted for a service that does not exist.
func TestServiceConfigPutUnknownService(t *testing.T) {
	h, user := newServiceConfigTestHandler(t)

	rec := httptest.NewRecorder()
	h.PutConfig(rec, configRequest(user, http.MethodPut, "/api/services/nope/config", "nope",
		`{"config":{"region":"us-east-1"}}`))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("PUT unknown service: got %d (%s), want 404", rec.Code, rec.Body.String())
	}

	// The rejected write must not have persisted an orphan row.
	rec = httptest.NewRecorder()
	h.GetConfig(rec, configRequest(user, http.MethodGet, "/api/services/nope/config", "nope", ""))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET after rejected PUT: got %d, want 404 (orphan row persisted)", rec.Code)
	}
}

// TestServiceConfigRequiresAuth rejects an unauthenticated request.
func TestServiceConfigRequiresAuth(t *testing.T) {
	h, _ := newServiceConfigTestHandler(t)
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/services/svc/config", nil)
	r.SetPathValue("serviceID", "svc")
	h.GetConfig(rec, r)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no user: got %d, want 401", rec.Code)
	}
}
