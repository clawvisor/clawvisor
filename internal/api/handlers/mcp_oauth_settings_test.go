package handlers

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/clawvisor/clawvisor/pkg/adapters"
	"github.com/clawvisor/clawvisor/pkg/adapters/mcpadapter"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// newServicesHandlerForOAuth builds a ServicesHandler with the minimum wiring
// needed for the MCP OAuth settings tests — no DB, no vault encryption, just
// the in-memory vault and a registry holding one MCP-OAuth adapter.
func newServicesHandlerForOAuth(t *testing.T) (*ServicesHandler, *memVault, *mcpadapter.MCPAdapter) {
	t.Helper()
	v := newMemVault()
	reg := adapters.NewRegistry()

	var spec mcpadapter.Spec
	spec.Service.ID = "notion-mcp"
	spec.Service.DisplayName = "Notion (MCP)"
	spec.MCP.Transport = "http"
	spec.MCP.Endpoint = "https://mcp.notion.com/mcp"
	spec.MCP.OAuth = &mcpadapter.MCPOAuthSpec{
		AuthorizeURL: "https://api.notion.com/v1/oauth/authorize",
		TokenURL:     "https://api.notion.com/v1/oauth/token",
	}
	adapter := mcpadapter.FromSpec(spec, &mcpadapter.HTTPTransport{Endpoint: spec.MCP.Endpoint})
	adapter.SetOAuthVault(v)
	reg.Register(adapter)

	h := &ServicesHandler{
		vault:      v,
		adapterReg: reg,
		logger:     slog.Default(),
	}
	return h, v, adapter
}

func asAdmin(req *http.Request) *http.Request {
	return req.WithContext(withUser(req.Context(), &store.User{ID: "admin"}))
}

// TestMCPOAuthSettings_RoundTrip exercises set → list → delete via the HTTP
// handlers and confirms each step is reflected in MCPAdapter.OAuthConfig().
func TestMCPOAuthSettings_RoundTrip(t *testing.T) {
	h, _, adapter := newServicesHandlerForOAuth(t)

	// Before any creds are set, OAuthConfig() returns nil — the catalog UI
	// would show "needs OAuth setup".
	if adapter.OAuthConfig() != nil {
		t.Fatal("OAuthConfig should be nil before credentials are stored")
	}

	// POST /api/system/mcp-oauth — admin saves client_id + client_secret.
	body := bytes.NewBufferString(`{"service_id":"notion-mcp","client_id":"cid-1","client_secret":"csec-1"}`)
	req := asAdmin(httptest.NewRequest("POST", "/api/system/mcp-oauth", body))
	w := httptest.NewRecorder()
	h.SetMCPOAuthCredential(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Adapter immediately picks up the new credentials (no restart).
	cfg := adapter.OAuthConfig()
	if cfg == nil {
		t.Fatal("OAuthConfig should be non-nil after SetMCPOAuthCredential")
	}
	if cfg.ClientID != "cid-1" {
		t.Errorf("ClientID = %q, want %q", cfg.ClientID, "cid-1")
	}
	if cfg.Endpoint.AuthURL != "https://api.notion.com/v1/oauth/authorize" {
		t.Errorf("endpoint not carried through from spec: %q", cfg.Endpoint.AuthURL)
	}

	// GET /api/system/mcp-oauth — settings UI lists configured services.
	req = asAdmin(httptest.NewRequest("GET", "/api/system/mcp-oauth", nil))
	w = httptest.NewRecorder()
	h.ListMCPOAuthCredentials(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("list: expected 200, got %d", w.Code)
	}
	var entries []adapters.MCPOAuthEntry
	if err := json.Unmarshal(w.Body.Bytes(), &entries); err != nil {
		t.Fatalf("list: bad json: %v", err)
	}
	if len(entries) != 1 || entries[0].ServiceID != "notion-mcp" || entries[0].ClientID != "cid-1" {
		t.Fatalf("list response unexpected: %+v", entries)
	}
	// Secret must never appear in list responses.
	if strings.Contains(w.Body.String(), "csec-1") {
		t.Errorf("client_secret leaked in list response: %s", w.Body.String())
	}

	// DELETE /api/system/mcp-oauth/notion-mcp.
	req = asAdmin(httptest.NewRequest("DELETE", "/api/system/mcp-oauth/notion-mcp", nil))
	req.SetPathValue("service_id", "notion-mcp")
	w = httptest.NewRecorder()
	h.DeleteMCPOAuthCredential(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("delete: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Adapter reverts to "needs setup" immediately.
	if adapter.OAuthConfig() != nil {
		t.Fatal("OAuthConfig should be nil again after delete")
	}
}

// TestMCPOAuthSettings_RejectsNonMCPService ensures admins can't accidentally
// seed credentials under a typo'd or non-MCP service ID.
func TestMCPOAuthSettings_RejectsNonMCPService(t *testing.T) {
	h, _, _ := newServicesHandlerForOAuth(t)

	body := bytes.NewBufferString(`{"service_id":"not-a-real-thing","client_id":"x","client_secret":"y"}`)
	req := asAdmin(httptest.NewRequest("POST", "/api/system/mcp-oauth", body))
	w := httptest.NewRecorder()
	h.SetMCPOAuthCredential(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for unknown service, got %d: %s", w.Code, w.Body.String())
	}
}

// TestMCPOAuthSettings_RejectsMissingFields covers input validation.
func TestMCPOAuthSettings_RejectsMissingFields(t *testing.T) {
	h, _, _ := newServicesHandlerForOAuth(t)

	cases := []string{
		`{"client_id":"x","client_secret":"y"}`,                    // missing service_id
		`{"service_id":"notion-mcp","client_secret":"y"}`,          // missing client_id
		`{"service_id":"notion-mcp","client_id":"x"}`,              // missing client_secret
		`{}`,                                                        // all missing
	}
	for _, b := range cases {
		req := asAdmin(httptest.NewRequest("POST", "/api/system/mcp-oauth", bytes.NewBufferString(b)))
		w := httptest.NewRecorder()
		h.SetMCPOAuthCredential(w, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("body %q: expected 400, got %d", b, w.Code)
		}
	}
}
