package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	mcpdefs "github.com/clawvisor/clawvisor/internal/adapters/definitions/mcp"
	"github.com/clawvisor/clawvisor/pkg/adapters"
	"github.com/clawvisor/clawvisor/pkg/adapters/mcpadapter"
	"github.com/clawvisor/clawvisor/pkg/adapters/mcpclient"
	"github.com/clawvisor/clawvisor/pkg/store/sqlite"
	"github.com/clawvisor/clawvisor/pkg/vault"
	"gopkg.in/yaml.v3"
)

// notionHTTPMock is a tiny in-process MCP server that speaks streamable
// HTTP and serves the same Notion-shaped responses NotionMockConfig provides
// over stdio. Used by the spec-driven test to exercise the prod spec's
// transport=http path without reaching mcp.notion.com.
type notionHTTPMock struct {
	sessionID  string
	expectAuth string
}

func (m *notionHTTPMock) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      int             `json:"id,omitempty"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if req.Method != "initialize" && req.Method != "notifications/initialized" {
		if got := r.Header.Get("Authorization"); got != m.expectAuth {
			http.Error(w, "auth mismatch", http.StatusUnauthorized)
			return
		}
		if got := r.Header.Get("Mcp-Session-Id"); got != m.sessionID {
			http.Error(w, "session mismatch", http.StatusBadRequest)
			return
		}
	}
	writeResp := func(result any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
	}
	switch req.Method {
	case "initialize":
		w.Header().Set("Mcp-Session-Id", m.sessionID)
		writeResp(map[string]any{
			"protocolVersion": "2025-06-18",
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "notion-mcp-mock"},
		})
	case "notifications/initialized":
		w.WriteHeader(http.StatusAccepted)
	case "tools/list":
		// Mirror the four tools from NotionMockConfig.
		writeResp(map[string]any{
			"tools": []mcpclient.Tool{
				{Name: "search", Description: "Search.", Annotations: map[string]any{"readOnlyHint": true}},
				{Name: "get_page", Description: "Get page.", Annotations: map[string]any{"readOnlyHint": true}},
				{Name: "create_page", Description: "Create page.", Annotations: map[string]any{"destructiveHint": true}},
				{Name: "notion-get-users", Description: "List or fetch user(s).", Annotations: map[string]any{"readOnlyHint": true}},
			},
		})
	case "tools/call":
		var p struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		_ = json.Unmarshal(req.Params, &p)
		var data any
		switch p.Name {
		case "notion-get-users":
			// Notion returns a {results:[...]} envelope; with user_id:"self"
			// the first entry is the authed user.
			if uid, _ := p.Arguments["user_id"].(string); uid != "self" {
				http.Error(w, "expected user_id=self for whoami", http.StatusBadRequest)
				return
			}
			data = map[string]any{
				"has_more": false,
				"results": []map[string]any{
					{"email": "user@example.com", "id": "u1", "name": "User", "type": "person"},
				},
			}
		case "get_page":
			pid, _ := p.Arguments["page_id"].(string)
			data = map[string]any{"id": pid, "url": "https://notion.so/" + pid}
		case "search":
			q, _ := p.Arguments["query"].(string)
			data = map[string]any{"results": []map[string]any{{"id": "page-abc", "title": "match " + q}}}
		default:
			http.Error(w, "unknown tool", http.StatusBadRequest)
			return
		}
		payload, _ := json.Marshal(data)
		writeResp(mcpclient.ToolResult{
			Content: []mcpclient.ToolContent{{Type: "text", Text: string(payload)}},
		})
	default:
		http.Error(w, "method not found", http.StatusNotFound)
	}
}

// memVault is a minimal in-memory vault for the prototype's E2E test.
type memVault struct {
	creds map[string]map[string][]byte // userID → serviceID → bytes
}

func newMemVault() *memVault { return &memVault{creds: map[string]map[string][]byte{}} }

func (m *memVault) Set(_ context.Context, userID, serviceID string, c []byte) error {
	if m.creds[userID] == nil {
		m.creds[userID] = map[string][]byte{}
	}
	m.creds[userID][serviceID] = c
	return nil
}
func (m *memVault) Get(_ context.Context, userID, serviceID string) ([]byte, error) {
	if u, ok := m.creds[userID]; ok {
		if c, ok := u[serviceID]; ok {
			return c, nil
		}
	}
	return nil, vault.ErrNotFound
}
func (m *memVault) Delete(_ context.Context, userID, serviceID string) error {
	if u, ok := m.creds[userID]; ok {
		delete(u, serviceID)
	}
	return nil
}
func (m *memVault) List(_ context.Context, userID string) ([]string, error) {
	out := []string{}
	for k := range m.creds[userID] {
		out = append(out, k)
	}
	return out, nil
}

// TestMCPNotionEndToEnd exercises the full inverted path:
//
//	executeAdapterRequest → MCPAdapter (in-process transport) → mock notion server
//	→ raw vendor-shaped JSON → gateway sanitization middleware → caller
//
// Asserts that the gateway's middleware (not the adapter) stripped HTML and
// truncated the long field — i.e., that the architectural inversion works.
// TestMCPNotionSpecDriven proves the "delete the per-service Go" path:
// load a YAML spec from embedded FS, swap in an in-process transport (so the
// test doesn't shell out), register, and exercise both whoami (the only
// per-service hook) and a regular tool call. This is what adding a new
// MCP-backed service to Clawvisor looks like in the inverted architecture:
// drop a *.mcp.yaml file, and the rest just works.
func TestMCPNotionSpecDriven(t *testing.T) {
	// Read the spec straight from the embedded FS — exactly how production
	// would load it. The spec declares transport=http pointing at
	// mcp.notion.com; the test redirects to an in-process httptest.Server
	// running a Notion-shaped MCP mock so the test stays hermetic.
	data, err := mcpdefs.FS.ReadFile("notion.mcp.yaml")
	if err != nil {
		t.Fatalf("read embedded spec: %v", err)
	}
	var spec mcpadapter.Spec
	if err := yaml.Unmarshal(data, &spec); err != nil {
		t.Fatalf("parse spec: %v", err)
	}
	if spec.Service.ID != "notion-mcp" {
		t.Fatalf("expected notion-mcp, got %q", spec.Service.ID)
	}
	if spec.MCP.Transport != "http" {
		t.Fatalf("expected http transport in spec, got %q", spec.MCP.Transport)
	}
	if spec.MCP.OAuth == nil {
		t.Fatalf("expected oauth declared in spec")
	}
	if spec.MCP.Whoami == nil || spec.MCP.Whoami.Tool == "" {
		t.Fatalf("whoami tool not declared in spec")
	}

	const accessToken = "oauth-access-token-xyz"
	srv := httptest.NewServer(&notionHTTPMock{
		sessionID:  "sess-spec-driven",
		expectAuth: "Bearer " + accessToken,
	})
	t.Cleanup(srv.Close)

	// Override the endpoint to point at the test server while keeping the
	// rest of the spec verbatim — including the oauth declaration.
	spec.MCP.Endpoint = srv.URL
	transport := &mcpadapter.HTTPTransport{Endpoint: spec.MCP.Endpoint}
	adapter := mcpadapter.FromSpec(spec, transport)

	// Provide system OAuth client_id / client_secret the way SetGoogleOAuthConfig
	// would, so MCPAdapter.OAuthConfig() returns non-nil. The OAuth code path
	// reads "__system__"/"mcp.oauth.notion" from the vault.
	v := newMemVault()
	clientCreds := []byte(`{"client_id":"cid","client_secret":"csec"}`)
	_ = v.Set(context.Background(), adapters.SystemUserID,
		adapters.SystemVaultKeyMCPOAuthPrefix+"notion-mcp", clientCreds)
	adapter.SetOAuthVault(v)

	if cfg := adapter.OAuthConfig(); cfg == nil {
		t.Fatal("OAuthConfig should be non-nil once client creds are in vault")
	}

	// Simulate the result of a completed OAuth code → token exchange: store
	// the standard token envelope as the user's credential. This is what
	// services.go's OAuthCallback would persist.
	credJSON := []byte(`{"access_token":"` + accessToken + `","refresh_token":"refresh-1"}`)
	_ = v.Set(context.Background(), "user-1", "notion-mcp", credJSON)

	// Whoami: the only per-service hook in the spec.
	identity, err := adapter.FetchIdentity(context.Background(), credJSON, nil)
	if err != nil {
		t.Fatalf("FetchIdentity: %v", err)
	}
	if identity != "user@example.com" {
		t.Fatalf("expected results[0].email from notion-get-users, got %q", identity)
	}

	// And regular execution still works through the same gateway path —
	// over HTTP to the (test) remote MCP server, with the OAuth-wrapped
	// http.Client supplying the Authorization header automatically.
	tools, err := adapter.DiscoverTools(context.Background(), credJSON)
	if err != nil {
		t.Fatalf("DiscoverTools: %v", err)
	}
	reg := adapters.NewRegistry()
	reg.Register(adapter)
	reg.RegisterForUser("user-1", adapter.ForUser(tools))

	result, err := executeAdapterRequest(
		context.Background(),
		v, reg, nil,
		"user-1", "notion-mcp", "get_page",
		map[string]any{"page_id": "page-zzz"},
		"",
	)
	if err != nil {
		t.Fatalf("executeAdapterRequest: %v", err)
	}
	dataMap, _ := result.Data.(map[string]any)
	if id, _ := dataMap["id"].(string); id != "page-zzz" {
		t.Fatalf("expected page-zzz, got %#v", dataMap)
	}

	// Service metadata picked up the YAML strings.
	meta := adapter.ServiceMetadata()
	if meta.DisplayName != "Notion" {
		t.Errorf("display name from spec not used: %q", meta.DisplayName)
	}
	if !meta.AutoIdentity {
		t.Errorf("AutoIdentity should be true when whoami is declared")
	}
}

// TestMCPNotionPersistenceRoundtrip is the load-bearing test for the v3.5
// design: discovery is persisted at activation, then hydrated lazily from
// the DB on cache miss (which is what happens after a server restart).
//
// Flow:
//  1. Activation simulation: discover tools, persist via store.UpsertMCPTools,
//     register a per-user clone in the registry.
//  2. Simulate a restart: build a *fresh* registry that knows nothing about
//     the user. Same store, same MCP adapter spec.
//  3. Wire the resolver to read tools from the store (mirroring defaults.go).
//  4. Call AllForUser on the fresh registry. The resolver should hit the
//     store, hydrate a per-user clone, and return it with the discovered
//     tools — without re-spawning the MCP server.
//
// This proves: catalog/gateway never need to "warm up" at startup. They
// pull from the DB on demand, on first call from each user.
func TestMCPPersistenceRoundtrip(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.New(ctx, filepath.Join(t.TempDir(), "mcp.db"))
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := sqlite.NewStore(db)
	user, err := st.CreateUser(ctx, "mcp-roundtrip@test.example", "hash")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	// Build a global adapter without any transport — the persistence test
	// doesn't exercise discovery, it exercises the registry+store roundtrip.
	var spec mcpadapter.Spec
	spec.Service.ID = "fake-mcp"
	spec.MCP.Transport = "http"
	spec.MCP.Endpoint = "https://example.invalid/mcp"
	globalAdapter := mcpadapter.FromSpec(spec, &mcpadapter.HTTPTransport{Endpoint: spec.MCP.Endpoint})

	// ── Phase 1: persist a synthetic tool list (simulates activation having
	// already run DiscoverTools and called UpsertMCPTools). ───────────────
	tools := []mcpclient.Tool{
		{Name: "alpha", Description: "Alpha.", Annotations: map[string]any{"readOnlyHint": true}},
		{Name: "beta", Description: "Beta.", Annotations: map[string]any{"readOnlyHint": true}},
		{Name: "gamma", Description: "Gamma.", Annotations: map[string]any{"destructiveHint": true}},
	}
	toolsJSON, _ := json.Marshal(tools)
	if err := st.UpsertMCPTools(ctx, user.ID, "fake-mcp", "default", toolsJSON); err != nil {
		t.Fatalf("UpsertMCPTools: %v", err)
	}

	// ── Phase 2: simulate restart by building a fresh registry. ───────────
	// In production this is what happens when the server reboots — the
	// in-memory userAdapters map is empty, but the DB still has the tools.
	freshReg := adapters.NewRegistry()
	freshReg.Register(globalAdapter) // global, no per-user state
	mcpByID := map[string]*mcpadapter.MCPAdapter{"fake-mcp": globalAdapter}
	freshReg.SetResolver(func(ctx context.Context, serviceID, userID string) (adapters.Adapter, bool) {
		mcp, ok := mcpByID[serviceID]
		if !ok {
			return nil, false
		}
		raw, err := st.GetMCPTools(ctx, userID, serviceID, "default")
		if err != nil {
			return nil, false
		}
		var loaded []mcpclient.Tool
		if err := json.Unmarshal(raw, &loaded); err != nil {
			return nil, false
		}
		return mcp.ForUser(loaded), true
	})

	// ── Phase 3: catalog asks AllForUser; resolver hydrates from DB. ──────
	all := freshReg.AllForUser(ctx, user.ID)
	var hydrated *mcpadapter.MCPAdapter
	for _, a := range all {
		if a.ServiceID() == "fake-mcp" {
			hydrated, _ = a.(*mcpadapter.MCPAdapter)
		}
	}
	if hydrated == nil {
		t.Fatal("AllForUser did not return MCP adapter for user")
	}
	if got := len(hydrated.SupportedActions()); got != len(tools) {
		t.Fatalf("expected %d hydrated tools after DB roundtrip, got %d: %v",
			len(tools), got, hydrated.SupportedActions())
	}
	if hydrated == globalAdapter {
		t.Fatal("resolver returned the global instance — should have built a per-user clone")
	}

	// ── Phase 4: subsequent calls hit the in-memory cache. ────────────────
	// Delete the DB row to prove it's not re-read.
	if err := st.DeleteMCPTools(ctx, user.ID, "fake-mcp", "default"); err != nil {
		t.Fatalf("DeleteMCPTools: %v", err)
	}
	all2 := freshReg.AllForUser(ctx, user.ID)
	var still *mcpadapter.MCPAdapter
	for _, a := range all2 {
		if a.ServiceID() == "fake-mcp" {
			still, _ = a.(*mcpadapter.MCPAdapter)
		}
	}
	if still == nil || len(still.SupportedActions()) != len(tools) {
		t.Fatalf("in-memory cache should serve subsequent calls without re-reading DB")
	}
}

// TestMCPPersistenceRoundtrip_AliasFromServiceMeta is the regression test
// for the resolver-alias bug: whoami-enabled adapters persist tools under
// the resolved alias (email, org name, etc.) rather than "default". The
// resolver in defaults.go has to look up the user's actual alias via
// service_meta rather than blindly querying for "default", or after a
// restart the catalog loses every MCP action.
func TestMCPPersistenceRoundtrip_AliasFromServiceMeta(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.New(ctx, filepath.Join(t.TempDir(), "mcp-alias.db"))
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := sqlite.NewStore(db)
	user, err := st.CreateUser(ctx, "alias-roundtrip@test.example", "hash")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	var spec mcpadapter.Spec
	spec.Service.ID = "notion-mcp"
	spec.MCP.Transport = "http"
	spec.MCP.Endpoint = "https://example.invalid/mcp"
	globalAdapter := mcpadapter.FromSpec(spec, &mcpadapter.HTTPTransport{Endpoint: spec.MCP.Endpoint})

	// Whoami resolved the user's identity to their email; activation
	// persisted tools + service_meta under that alias, NOT "default".
	const aliasFromWhoami = "user-at-example-com"
	tools := []mcpclient.Tool{{Name: "notion-search", Description: "Search."}}
	toolsJSON, _ := json.Marshal(tools)
	if err := st.UpsertMCPTools(ctx, user.ID, "notion-mcp", aliasFromWhoami, toolsJSON); err != nil {
		t.Fatalf("UpsertMCPTools: %v", err)
	}
	if err := st.UpsertServiceMeta(ctx, user.ID, "notion-mcp", aliasFromWhoami, time.Now()); err != nil {
		t.Fatalf("UpsertServiceMeta: %v", err)
	}

	// Fresh registry (simulating server restart). Resolver mirrors defaults.go:
	// consult service_meta for the user's alias, then look up tools under it.
	// Asserting "default" would not find anything; we want the alias-aware
	// path to succeed.
	freshReg := adapters.NewRegistry()
	freshReg.Register(globalAdapter)
	mcpByID := map[string]*mcpadapter.MCPAdapter{"notion-mcp": globalAdapter}
	freshReg.SetResolver(func(ctx context.Context, serviceID, userID string) (adapters.Adapter, bool) {
		mcp, ok := mcpByID[serviceID]
		if !ok {
			return nil, false
		}
		aliases := []string{"default"}
		if metas, err := st.ListServiceMetas(ctx, userID); err == nil {
			aliases = aliases[:0]
			for _, m := range metas {
				if m.ServiceID == serviceID {
					aliases = append(aliases, m.Alias)
				}
			}
			if len(aliases) == 0 {
				aliases = []string{"default"}
			}
		}
		for _, alias := range aliases {
			raw, err := st.GetMCPTools(ctx, userID, serviceID, alias)
			if err != nil {
				continue
			}
			var loaded []mcpclient.Tool
			if err := json.Unmarshal(raw, &loaded); err != nil {
				continue
			}
			return mcp.ForUser(loaded), true
		}
		return nil, false
	})

	all := freshReg.AllForUser(ctx, user.ID)
	var hydrated *mcpadapter.MCPAdapter
	for _, a := range all {
		if a.ServiceID() == "notion-mcp" {
			hydrated, _ = a.(*mcpadapter.MCPAdapter)
		}
	}
	if hydrated == nil {
		t.Fatal("resolver returned no adapter; check service_meta alias path")
	}
	if got := len(hydrated.SupportedActions()); got != 1 {
		t.Fatalf("expected 1 tool after alias-aware hydration, got %d (resolver likely fell back to 'default')", got)
	}
}
