package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/clawvisor/clawvisor/internal/events"
	"github.com/clawvisor/clawvisor/internal/intent"
	"github.com/clawvisor/clawvisor/pkg/adapters"
	"github.com/clawvisor/clawvisor/pkg/config"
	"github.com/clawvisor/clawvisor/pkg/store"
	"github.com/clawvisor/clawvisor/pkg/vault"
)

// ── Test mocks ──────────────────────────────────────────────────────────────

// mockLocalProvider is a configurable LocalServiceProvider for tests.
type mockLocalProvider struct {
	services []LocalCatalogService
	err      error
}

func (m *mockLocalProvider) ActiveLocalServices(_ context.Context, _ string) ([]LocalCatalogService, error) {
	return m.services, m.err
}

// mockLocalExecutor is a configurable LocalServiceExecutor for tests.
type mockLocalExecutor struct {
	result  *adapters.Result
	err     error
	called  bool
	service string
	action  string
}

func (m *mockLocalExecutor) Execute(_ context.Context, _, service, action string, _ map[string]any) (*adapters.Result, error) {
	m.called = true
	m.service = service
	m.action = action
	return m.result, m.err
}

// localTestStore is a minimal store for local service handler tests.
type localTestStore struct {
	store.Store // embed nil interface; only override needed methods

	tasks          map[string]*store.Task
	restrictions   map[string]*store.Restriction
	auditEntries   []*store.AuditEntry
	gatewayLogs    []*store.GatewayRequestLog
	serviceMetas   []*store.ServiceMeta
	chainFacts     []*store.ChainFact
	requestCounts  map[string]int
}

func newLocalTestStore() *localTestStore {
	return &localTestStore{
		tasks:         make(map[string]*store.Task),
		restrictions:  make(map[string]*store.Restriction),
		requestCounts: make(map[string]int),
	}
}

func (s *localTestStore) GetTask(_ context.Context, id string) (*store.Task, error) {
	t, ok := s.tasks[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return t, nil
}

func (s *localTestStore) MatchRestriction(_ context.Context, _, _, _ string) (*store.Restriction, error) {
	return nil, nil
}

func (s *localTestStore) LogAudit(_ context.Context, entry *store.AuditEntry) error {
	s.auditEntries = append(s.auditEntries, entry)
	return nil
}

func (s *localTestStore) LogGatewayRequest(_ context.Context, entry *store.GatewayRequestLog) error {
	s.gatewayLogs = append(s.gatewayLogs, entry)
	return nil
}

func (s *localTestStore) IncrementTaskRequestCount(_ context.Context, id string) error {
	s.requestCounts[id]++
	return nil
}

func (s *localTestStore) GetAuditEntryByRequestID(_ context.Context, _, _ string) (*store.AuditEntry, error) {
	return nil, store.ErrNotFound
}

func (s *localTestStore) ListChainFacts(_ context.Context, _, _ string, _ int) ([]*store.ChainFact, error) {
	return s.chainFacts, nil
}

func (s *localTestStore) SaveChainFacts(_ context.Context, _ []*store.ChainFact) error {
	return nil
}

func (s *localTestStore) ChainFactValueExists(_ context.Context, _, _, _ string) (bool, error) {
	return false, nil
}

func (s *localTestStore) ListServiceMetas(_ context.Context, _ string) ([]*store.ServiceMeta, error) {
	return s.serviceMetas, nil
}

// mockVerifier allows tests to control intent verification outcomes.
type mockVerifier struct {
	verdict *intent.VerificationVerdict
	err     error
	called  bool
}

func (m *mockVerifier) Verify(_ context.Context, _ intent.VerifyRequest) (*intent.VerificationVerdict, error) {
	m.called = true
	return m.verdict, m.err
}

// testVault is a minimal vault for tests — never stores or retrieves anything.
type testVault struct{}

func (testVault) Set(_ context.Context, _, _ string, _ []byte) error    { return nil }
func (testVault) Get(_ context.Context, _, _ string) ([]byte, error)    { return nil, vault.ErrNotFound }
func (testVault) Delete(_ context.Context, _, _ string) error           { return nil }
func (testVault) List(_ context.Context, _ string) ([]string, error)    { return nil, nil }

// testServices returns a standard set of local services for testing.
func testServices() []LocalCatalogService {
	return []LocalCatalogService{
		{
			ServiceID:   "files",
			DaemonName:  "my-mac",
			Name:        "File System",
			Description: "Read and write files on the local filesystem.",
			Actions: []LocalCatalogAction{
				{ID: "read_file", Name: "Read File", Description: "Read a file", Params: []LocalCatalogParam{
					{Name: "path", Type: "string", Required: true, Description: "File path"},
				}},
				{ID: "write_file", Name: "Write File", Description: "Write a file", Params: []LocalCatalogParam{
					{Name: "path", Type: "string", Required: true, Description: "File path"},
					{Name: "content", Type: "string", Required: true, Description: "File content"},
				}},
				{ID: "list_dir", Name: "List Directory", Description: "List directory contents"},
			},
		},
		{
			ServiceID:   "browser",
			DaemonName:  "my-mac",
			Name:        "Browser",
			Description: "Browser navigation and automation.",
			Actions: []LocalCatalogAction{
				{ID: "navigate", Name: "Navigate", Description: "Navigate to a URL"},
				{ID: "screenshot", Name: "Screenshot", Description: "Take a screenshot"},
			},
		},
	}
}

func withAgent(ctx context.Context, agent *store.Agent) context.Context {
	return store.WithAgent(ctx, agent)
}

var testAgent = &store.Agent{ID: "agent-1", UserID: "user-1", Name: "test-agent"}

// ── Gateway: validateLocalAction unit tests ─────────────────────────────────

func TestValidateLocalAction_ValidAction(t *testing.T) {
	h := &GatewayHandler{
		localSvcProvider: &mockLocalProvider{services: testServices()},
	}
	err := h.validateLocalAction(context.Background(), "user-1", "local.files", "read_file")
	if err != nil {
		t.Fatalf("expected no error for valid action, got: %v", err)
	}
}

func TestValidateLocalAction_InvalidAction(t *testing.T) {
	h := &GatewayHandler{
		localSvcProvider: &mockLocalProvider{services: testServices()},
	}
	err := h.validateLocalAction(context.Background(), "user-1", "local.files", "delete_file")
	if err == nil {
		t.Fatal("expected error for invalid action")
	}
	if !strings.Contains(err.Error(), "delete_file") {
		t.Fatalf("error should mention the invalid action, got: %v", err)
	}
	if !strings.Contains(err.Error(), "read_file") {
		t.Fatalf("error should list available actions, got: %v", err)
	}
}

func TestValidateLocalAction_UnknownService(t *testing.T) {
	h := &GatewayHandler{
		localSvcProvider: &mockLocalProvider{services: testServices()},
	}
	// Unknown service returns nil — let execution handle the error.
	err := h.validateLocalAction(context.Background(), "user-1", "local.unknown", "anything")
	if err != nil {
		t.Fatalf("expected nil for unknown service (deferred to execution), got: %v", err)
	}
}

func TestValidateLocalAction_ProviderError(t *testing.T) {
	h := &GatewayHandler{
		localSvcProvider: &mockLocalProvider{err: fmt.Errorf("provider down")},
	}
	// Provider errors return nil — let execution handle it.
	err := h.validateLocalAction(context.Background(), "user-1", "local.files", "read_file")
	if err != nil {
		t.Fatalf("expected nil on provider error (deferred to execution), got: %v", err)
	}
}

func TestValidateLocalAction_DifferentService(t *testing.T) {
	h := &GatewayHandler{
		localSvcProvider: &mockLocalProvider{services: testServices()},
	}
	// "navigate" exists on browser but not on files.
	err := h.validateLocalAction(context.Background(), "user-1", "local.files", "navigate")
	if err == nil {
		t.Fatal("expected error for action that exists on a different service")
	}
}

// ── Gateway: HandleRequest integration tests for local services ─────────────

func newTestGatewayHandler(st *localTestStore, provider *mockLocalProvider, executor *mockLocalExecutor, verifier *mockVerifier) *GatewayHandler {
	h := NewGatewayHandler(
		st,
		testVault{},
		adapters.NewRegistry(),
		nil,                  // notifier
		verifier,             // intent verifier
		intent.NoopExtractor{}, // extractor
		config.Config{},
		slog.Default(),
		"http://localhost:9090",
		events.NewHub(),
	)
	if provider != nil {
		h.SetLocalServiceProvider(provider)
	}
	if executor != nil {
		h.SetLocalServiceExecutor(executor)
	}
	return h
}

func makeGatewayRequest(t *testing.T, h *GatewayHandler, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(body)
	req := httptest.NewRequest("POST", "/api/gateway/request", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(withAgent(req.Context(), testAgent))
	w := httptest.NewRecorder()
	h.HandleRequest(w, req)
	return w
}

func TestGateway_LocalService_UnknownAction_Rejected(t *testing.T) {
	st := newLocalTestStore()
	st.tasks["task-1"] = &store.Task{
		ID: "task-1", UserID: "user-1", AgentID: "agent-1", Status: "active",
		AuthorizedActions: []store.TaskAction{
			{Service: "local.files", Action: "*", AutoExecute: true},
		},
	}

	provider := &mockLocalProvider{services: testServices()}
	executor := &mockLocalExecutor{result: &adapters.Result{Summary: "ok"}}
	verifier := &mockVerifier{} // no verification needed — should be rejected before

	h := newTestGatewayHandler(st, provider, executor, verifier)

	w := makeGatewayRequest(t, h, map[string]any{
		"service": "local.files",
		"action":  "delete_file",
		"reason":  "delete a temp file",
		"task_id": "task-1",
	})

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["code"] != "UNKNOWN_ACTION" {
		t.Fatalf("expected UNKNOWN_ACTION code, got %v", resp["code"])
	}
	if executor.called {
		t.Fatal("executor should not have been called for unknown action")
	}
}

func TestGateway_LocalService_ValidAction_Executes(t *testing.T) {
	st := newLocalTestStore()
	st.tasks["task-1"] = &store.Task{
		ID: "task-1", UserID: "user-1", AgentID: "agent-1", Status: "active",
		AuthorizedActions: []store.TaskAction{
			{Service: "local.files", Action: "read_file", AutoExecute: true},
		},
	}

	provider := &mockLocalProvider{services: testServices()}
	executor := &mockLocalExecutor{result: &adapters.Result{Summary: "file contents here"}}
	verifier := &mockVerifier{
		verdict: &intent.VerificationVerdict{
			Allow: true, ParamScope: "ok", ReasonCoherence: "ok", ExtractContext: true,
		},
	}

	h := newTestGatewayHandler(st, provider, executor, verifier)

	w := makeGatewayRequest(t, h, map[string]any{
		"service": "local.files",
		"action":  "read_file",
		"reason":  "read config file",
		"task_id": "task-1",
		"params":  map[string]any{"path": "/etc/hosts"},
	})

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if !executor.called {
		t.Fatal("executor should have been called")
	}
	if executor.action != "read_file" {
		t.Fatalf("executor called with action %q, want read_file", executor.action)
	}
}

func TestGateway_LocalService_IntentVerification_Blocks(t *testing.T) {
	st := newLocalTestStore()
	st.tasks["task-1"] = &store.Task{
		ID: "task-1", UserID: "user-1", AgentID: "agent-1", Status: "active",
		AuthorizedActions: []store.TaskAction{
			{Service: "local.files", Action: "*", AutoExecute: true},
		},
	}

	provider := &mockLocalProvider{services: testServices()}
	executor := &mockLocalExecutor{result: &adapters.Result{Summary: "ok"}}
	verifier := &mockVerifier{
		verdict: &intent.VerificationVerdict{
			Allow:           false,
			ParamScope:      "violation",
			ReasonCoherence: "incoherent",
			Explanation:     "Request does not match task purpose.",
		},
	}

	h := newTestGatewayHandler(st, provider, executor, verifier)

	w := makeGatewayRequest(t, h, map[string]any{
		"service": "local.files",
		"action":  "write_file",
		"reason":  "writing something suspicious",
		"task_id": "task-1",
		"params":  map[string]any{"path": "/etc/passwd", "content": "hacked"},
	})

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 (restricted status), got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["status"] != "restricted" {
		t.Fatalf("expected restricted status, got %v", resp["status"])
	}
	if executor.called {
		t.Fatal("executor should NOT have been called when intent verification fails")
	}
	if !verifier.called {
		t.Fatal("verifier should have been called for local service")
	}
}

func TestGateway_LocalService_PlannedCall_SkipsVerification(t *testing.T) {
	st := newLocalTestStore()
	st.tasks["task-1"] = &store.Task{
		ID: "task-1", UserID: "user-1", AgentID: "agent-1", Status: "active",
		AuthorizedActions: []store.TaskAction{
			{Service: "local.files", Action: "read_file", AutoExecute: true},
		},
		PlannedCalls: []store.PlannedCall{
			{Service: "local.files", Action: "read_file", Params: map[string]any{"path": "/etc/hosts"}, Reason: "Read hosts file"},
		},
	}

	provider := &mockLocalProvider{services: testServices()}
	executor := &mockLocalExecutor{result: &adapters.Result{Summary: "127.0.0.1 localhost"}}
	verifier := &mockVerifier{
		// If this were called, it would block. But planned call should skip it.
		verdict: &intent.VerificationVerdict{
			Allow: false, ParamScope: "violation", Explanation: "should not be reached",
		},
	}

	h := newTestGatewayHandler(st, provider, executor, verifier)

	w := makeGatewayRequest(t, h, map[string]any{
		"service": "local.files",
		"action":  "read_file",
		"reason":  "Read hosts file",
		"task_id": "task-1",
		"params":  map[string]any{"path": "/etc/hosts"},
	})

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if !executor.called {
		t.Fatal("executor should have been called — planned call matches")
	}
	// Verifier should NOT have been called (planned call bypasses it).
	if verifier.called {
		t.Fatal("verifier should not be called when planned call matches")
	}
}

func TestGateway_LocalService_NoProvider_SkipsValidation(t *testing.T) {
	st := newLocalTestStore()
	st.tasks["task-1"] = &store.Task{
		ID: "task-1", UserID: "user-1", AgentID: "agent-1", Status: "active",
		AuthorizedActions: []store.TaskAction{
			{Service: "local.files", Action: "read_file", AutoExecute: true},
		},
	}

	// No provider — self-hosted mode. Should skip action validation.
	executor := &mockLocalExecutor{result: &adapters.Result{Summary: "ok"}}
	verifier := &mockVerifier{
		verdict: &intent.VerificationVerdict{Allow: true, ParamScope: "ok", ReasonCoherence: "ok"},
	}

	h := newTestGatewayHandler(st, nil, executor, verifier)

	w := makeGatewayRequest(t, h, map[string]any{
		"service": "local.files",
		"action":  "read_file",
		"reason":  "read a file",
		"task_id": "task-1",
	})

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if !executor.called {
		t.Fatal("executor should have been called without provider")
	}
}

func TestGateway_LocalService_NoExecutor_ReturnsError(t *testing.T) {
	st := newLocalTestStore()
	st.tasks["task-1"] = &store.Task{
		ID: "task-1", UserID: "user-1", AgentID: "agent-1", Status: "active",
		AuthorizedActions: []store.TaskAction{
			{Service: "local.files", Action: "read_file", AutoExecute: true},
		},
	}

	provider := &mockLocalProvider{services: testServices()}
	verifier := &mockVerifier{
		verdict: &intent.VerificationVerdict{Allow: true, ParamScope: "ok", ReasonCoherence: "ok"},
	}

	// No executor configured.
	h := newTestGatewayHandler(st, provider, nil, verifier)

	w := makeGatewayRequest(t, h, map[string]any{
		"service": "local.files",
		"action":  "read_file",
		"reason":  "read a file",
		"task_id": "task-1",
	})

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 (error status), got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["code"] != "LOCAL_SERVICE_UNAVAILABLE" {
		t.Fatalf("expected LOCAL_SERVICE_UNAVAILABLE, got %v", resp["code"])
	}
}

func TestGateway_LocalService_OutOfScope_Rejected(t *testing.T) {
	st := newLocalTestStore()
	st.tasks["task-1"] = &store.Task{
		ID: "task-1", UserID: "user-1", AgentID: "agent-1", Status: "active",
		AuthorizedActions: []store.TaskAction{
			{Service: "local.files", Action: "read_file", AutoExecute: true},
		},
	}

	provider := &mockLocalProvider{services: testServices()}
	executor := &mockLocalExecutor{result: &adapters.Result{Summary: "ok"}}
	verifier := &mockVerifier{}

	h := newTestGatewayHandler(st, provider, executor, verifier)

	// write_file is a valid action on the service, but NOT in the task scope.
	w := makeGatewayRequest(t, h, map[string]any{
		"service": "local.files",
		"action":  "write_file",
		"reason":  "write a file",
		"task_id": "task-1",
	})

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	// Out of scope returns pending_scope_expansion for session tasks.
	if resp["status"] != "pending_scope_expansion" {
		t.Fatalf("expected pending_scope_expansion, got %v", resp["status"])
	}
	if executor.called {
		t.Fatal("executor should not have been called for out-of-scope action")
	}
}

// ── Tasks: validateLocalService unit tests ──────────────────────────────────

func TestValidateLocalService_ValidServiceAndAction(t *testing.T) {
	h := &TasksHandler{
		localSvcProvider: &mockLocalProvider{services: testServices()},
	}
	err := h.validateLocalService(context.Background(), "user-1", "local.files", "read_file")
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
}

func TestValidateLocalService_ValidServiceWildcardAction(t *testing.T) {
	h := &TasksHandler{
		localSvcProvider: &mockLocalProvider{services: testServices()},
	}
	err := h.validateLocalService(context.Background(), "user-1", "local.files", "*")
	if err != nil {
		t.Fatalf("expected no error for wildcard action, got: %v", err)
	}
}

func TestValidateLocalService_ValidServiceInvalidAction(t *testing.T) {
	h := &TasksHandler{
		localSvcProvider: &mockLocalProvider{services: testServices()},
	}
	err := h.validateLocalService(context.Background(), "user-1", "local.files", "delete_file")
	if err == nil {
		t.Fatal("expected error for invalid action")
	}
	if !strings.Contains(err.Error(), "delete_file") {
		t.Fatalf("error should mention the invalid action, got: %v", err)
	}
	if !strings.Contains(err.Error(), "read_file") {
		t.Fatalf("error should list available actions, got: %v", err)
	}
}

func TestValidateLocalService_UnknownService(t *testing.T) {
	h := &TasksHandler{
		localSvcProvider: &mockLocalProvider{services: testServices()},
	}
	err := h.validateLocalService(context.Background(), "user-1", "local.unknown", "anything")
	if err == nil {
		t.Fatal("expected error for unknown service")
	}
	if !strings.Contains(err.Error(), "not enabled") {
		t.Fatalf("error should say not enabled, got: %v", err)
	}
}

func TestValidateLocalService_NoProvider(t *testing.T) {
	h := &TasksHandler{
		localSvcProvider: nil,
	}
	err := h.validateLocalService(context.Background(), "user-1", "local.files", "read_file")
	if err != nil {
		t.Fatalf("expected nil when no provider (self-hosted), got: %v", err)
	}
}

func TestValidateLocalService_ProviderError(t *testing.T) {
	h := &TasksHandler{
		localSvcProvider: &mockLocalProvider{err: fmt.Errorf("connection refused")},
	}
	err := h.validateLocalService(context.Background(), "user-1", "local.files", "read_file")
	if err == nil {
		t.Fatal("expected error when provider fails")
	}
	if !strings.Contains(err.Error(), "unable to verify") {
		t.Fatalf("error should indicate verification failure, got: %v", err)
	}
}

func TestValidateLocalService_ActionOnDifferentService(t *testing.T) {
	h := &TasksHandler{
		localSvcProvider: &mockLocalProvider{services: testServices()},
	}
	// "navigate" exists on browser, not files.
	err := h.validateLocalService(context.Background(), "user-1", "local.files", "navigate")
	if err == nil {
		t.Fatal("expected error for action that belongs to a different service")
	}
}

func TestValidateLocalService_EmptyAction(t *testing.T) {
	h := &TasksHandler{
		localSvcProvider: &mockLocalProvider{services: testServices()},
	}
	// Empty action should pass (treated like wildcard).
	err := h.validateLocalService(context.Background(), "user-1", "local.files", "")
	if err != nil {
		t.Fatalf("expected no error for empty action, got: %v", err)
	}
}

// ── Catalog: local service rendering tests ──────────────────────────────────

func newTestSkillHandler(provider *mockLocalProvider) *SkillHandler {
	h := NewSkillHandler(
		newLocalTestStore(),
		testVault{},
		adapters.NewRegistry(),
		slog.Default(),
	)
	if provider != nil {
		h.SetLocalServiceProvider(provider)
	}
	return h
}

func TestCatalogOverview_IncludesLocalServices(t *testing.T) {
	provider := &mockLocalProvider{services: testServices()}
	h := newTestSkillHandler(provider)

	var buf strings.Builder
	entries := []*catalogEntry{} // no cloud services
	adapterMeta := map[string]adapters.ServiceMetadata{}
	h.writeCatalogOverview(&buf, context.Background(), entries, adapterMeta, "user-1")

	output := buf.String()

	// Should include the local services section.
	if !strings.Contains(output, "Local Services") {
		t.Fatal("catalog should contain Local Services section")
	}
	if !strings.Contains(output, "File System") {
		t.Fatalf("catalog should list File System service, got:\n%s", output)
	}
	if !strings.Contains(output, "Browser") {
		t.Fatalf("catalog should list Browser service, got:\n%s", output)
	}
	if !strings.Contains(output, "local.files") {
		t.Fatal("catalog should show service ID with local. prefix")
	}
	if !strings.Contains(output, "read_file") {
		t.Fatal("catalog should list actions")
	}
	if !strings.Contains(output, "one daemon at a time") {
		t.Fatal("catalog should mention single-daemon constraint")
	}
}

func TestCatalogOverview_NoLocalServices_OmitsSection(t *testing.T) {
	provider := &mockLocalProvider{services: nil}
	h := newTestSkillHandler(provider)

	var buf strings.Builder
	h.writeCatalogOverview(&buf, context.Background(), nil, nil, "user-1")

	if strings.Contains(buf.String(), "Local Services") {
		t.Fatal("catalog should not contain Local Services section when no services exist")
	}
}

func TestCatalogOverview_NoProvider_OmitsSection(t *testing.T) {
	h := newTestSkillHandler(nil)

	var buf strings.Builder
	h.writeCatalogOverview(&buf, context.Background(), nil, nil, "user-1")

	if strings.Contains(buf.String(), "Local Services") {
		t.Fatal("catalog should not contain Local Services section when provider is nil")
	}
}

func TestCatalogOverview_LocalServicesWithNoCloudServices(t *testing.T) {
	provider := &mockLocalProvider{services: testServices()}
	h := newTestSkillHandler(provider)

	var buf strings.Builder
	h.writeCatalogOverview(&buf, context.Background(), nil, nil, "user-1")

	output := buf.String()
	// Should still show local services even with no cloud services.
	if !strings.Contains(output, "Local Services") {
		t.Fatal("local services should appear even when no cloud services are connected")
	}
	// Should show the "no cloud services" message.
	if !strings.Contains(output, "No cloud services") {
		t.Fatal("should show no cloud services message")
	}
}

func TestCatalogOverview_ProviderError_ShowsCloudOnly(t *testing.T) {
	provider := &mockLocalProvider{err: fmt.Errorf("daemon offline")}
	h := newTestSkillHandler(provider)

	var buf strings.Builder
	h.writeCatalogOverview(&buf, context.Background(), nil, nil, "user-1")

	// Should not crash, and should not show Local Services section.
	if strings.Contains(buf.String(), "Local Services") {
		t.Fatal("should not show Local Services section when provider errors")
	}
}

func TestCatalogDetail_LocalService(t *testing.T) {
	provider := &mockLocalProvider{services: testServices()}
	h := newTestSkillHandler(provider)

	var buf strings.Builder
	h.writeLocalServiceDetail(&buf, context.Background(), "local.files", "user-1")

	output := buf.String()
	if !strings.Contains(output, "File System") {
		t.Fatalf("detail should show service name, got:\n%s", output)
	}
	if !strings.Contains(output, "local.files") {
		t.Fatal("detail should show service ID")
	}
	if !strings.Contains(output, "my-mac") {
		t.Fatal("detail should show daemon name")
	}
	if !strings.Contains(output, "read_file") {
		t.Fatal("detail should list actions")
	}
	if !strings.Contains(output, "path") {
		t.Fatal("detail should list params")
	}
	if !strings.Contains(output, "**required**") {
		t.Fatal("detail should mark required params")
	}
}

func TestCatalogDetail_LocalService_NotFound(t *testing.T) {
	provider := &mockLocalProvider{services: testServices()}
	h := newTestSkillHandler(provider)

	var buf strings.Builder
	h.writeLocalServiceDetail(&buf, context.Background(), "local.unknown", "user-1")

	if !strings.Contains(buf.String(), "not enabled") {
		t.Fatalf("should say service not enabled, got: %s", buf.String())
	}
}

func TestCatalogDetail_LocalService_NoProvider(t *testing.T) {
	h := newTestSkillHandler(nil)

	var buf strings.Builder
	h.writeLocalServiceDetail(&buf, context.Background(), "local.files", "user-1")

	if !strings.Contains(buf.String(), "not available") {
		t.Fatalf("should say not available, got: %s", buf.String())
	}
}

func TestCatalogDetail_RoutesLocalPrefix(t *testing.T) {
	provider := &mockLocalProvider{services: testServices()}
	h := newTestSkillHandler(provider)

	var buf strings.Builder
	// writeServiceDetail should detect local. prefix and route to writeLocalServiceDetail.
	entries := []*catalogEntry{}
	adapterMeta := map[string]adapters.ServiceMetadata{}
	h.writeServiceDetail(&buf, context.Background(), "local.files", entries, adapterMeta, "user-1")

	if !strings.Contains(buf.String(), "File System") {
		t.Fatal("writeServiceDetail should route local.* to writeLocalServiceDetail")
	}
}

func TestCatalogDetail_NonLocal_DoesNotRoute(t *testing.T) {
	h := newTestSkillHandler(nil)

	var buf strings.Builder
	entries := []*catalogEntry{}
	h.writeServiceDetail(&buf, context.Background(), "google.gmail", entries, nil, "user-1")

	// Should say not activated (not route to local handler).
	if strings.Contains(buf.String(), "not available in this deployment") {
		t.Fatal("non-local service should not be routed to local handler")
	}
	if !strings.Contains(buf.String(), "not activated") {
		t.Fatalf("should say not activated, got: %s", buf.String())
	}
}

// ── Integration: audit trail for local service requests ─────────────────────

func TestGateway_LocalService_AuditLogged(t *testing.T) {
	st := newLocalTestStore()
	st.tasks["task-1"] = &store.Task{
		ID: "task-1", UserID: "user-1", AgentID: "agent-1", Status: "active",
		AuthorizedActions: []store.TaskAction{
			{Service: "local.files", Action: "read_file", AutoExecute: true},
		},
	}

	provider := &mockLocalProvider{services: testServices()}
	executor := &mockLocalExecutor{result: &adapters.Result{Summary: "file contents"}}
	verifier := &mockVerifier{
		verdict: &intent.VerificationVerdict{Allow: true, ParamScope: "ok", ReasonCoherence: "ok"},
	}

	h := newTestGatewayHandler(st, provider, executor, verifier)
	_ = makeGatewayRequest(t, h, map[string]any{
		"service": "local.files",
		"action":  "read_file",
		"reason":  "read a config file",
		"task_id": "task-1",
	})

	if len(st.auditEntries) == 0 {
		t.Fatal("expected at least one audit entry")
	}
	entry := st.auditEntries[len(st.auditEntries)-1]
	if entry.Service != "local.files" {
		t.Fatalf("audit entry should have service local.files, got %s", entry.Service)
	}
	if entry.Decision != "execute" {
		t.Fatalf("audit entry should have decision execute, got %s", entry.Decision)
	}
}

func TestGateway_LocalService_UnknownAction_AuditLogged(t *testing.T) {
	st := newLocalTestStore()
	st.tasks["task-1"] = &store.Task{
		ID: "task-1", UserID: "user-1", AgentID: "agent-1", Status: "active",
		AuthorizedActions: []store.TaskAction{
			{Service: "local.files", Action: "*", AutoExecute: true},
		},
	}

	provider := &mockLocalProvider{services: testServices()}
	h := newTestGatewayHandler(st, provider, nil, &mockVerifier{})

	_ = makeGatewayRequest(t, h, map[string]any{
		"service": "local.files",
		"action":  "nonexistent",
		"reason":  "testing",
		"task_id": "task-1",
	})

	if len(st.auditEntries) == 0 {
		t.Fatal("expected audit entry for unknown action")
	}
	entry := st.auditEntries[0]
	if entry.Decision != "unknown_action" {
		t.Fatalf("audit decision should be unknown_action, got %s", entry.Decision)
	}
}

// ── Edge cases ──────────────────────────────────────────────────────────────

func TestGateway_LocalService_ExecutorError_ReturnsError(t *testing.T) {
	st := newLocalTestStore()
	st.tasks["task-1"] = &store.Task{
		ID: "task-1", UserID: "user-1", AgentID: "agent-1", Status: "active",
		AuthorizedActions: []store.TaskAction{
			{Service: "local.files", Action: "read_file", AutoExecute: true},
		},
	}

	provider := &mockLocalProvider{services: testServices()}
	executor := &mockLocalExecutor{err: fmt.Errorf("daemon timed out")}
	verifier := &mockVerifier{
		verdict: &intent.VerificationVerdict{Allow: true, ParamScope: "ok", ReasonCoherence: "ok"},
	}

	h := newTestGatewayHandler(st, provider, executor, verifier)

	w := makeGatewayRequest(t, h, map[string]any{
		"service": "local.files",
		"action":  "read_file",
		"reason":  "read a file",
		"task_id": "task-1",
	})

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 (error status), got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["status"] != "error" {
		t.Fatalf("expected error status, got %v", resp["status"])
	}
}

func TestGateway_LocalService_WildcardTask_ValidAction(t *testing.T) {
	st := newLocalTestStore()
	st.tasks["task-1"] = &store.Task{
		ID: "task-1", UserID: "user-1", AgentID: "agent-1", Status: "active",
		AuthorizedActions: []store.TaskAction{
			{Service: "local.files", Action: "*", AutoExecute: true},
		},
	}

	provider := &mockLocalProvider{services: testServices()}
	executor := &mockLocalExecutor{result: &adapters.Result{Summary: "ok"}}
	verifier := &mockVerifier{
		verdict: &intent.VerificationVerdict{Allow: true, ParamScope: "ok", ReasonCoherence: "ok"},
	}

	h := newTestGatewayHandler(st, provider, executor, verifier)

	// All three valid actions should work with wildcard scope.
	for _, action := range []string{"read_file", "write_file", "list_dir"} {
		executor.called = false
		w := makeGatewayRequest(t, h, map[string]any{
			"service": "local.files",
			"action":  action,
			"reason":  "testing " + action,
			"task_id": "task-1",
		})
		if w.Code != http.StatusOK {
			t.Fatalf("action %s: expected 200, got %d: %s", action, w.Code, w.Body.String())
		}
		if !executor.called {
			t.Fatalf("action %s: executor should have been called", action)
		}
	}
}

func TestCatalogOverview_ActionParams(t *testing.T) {
	provider := &mockLocalProvider{services: testServices()}
	h := newTestSkillHandler(provider)

	var buf strings.Builder
	h.writeCatalogOverview(&buf, context.Background(), nil, nil, "user-1")

	output := buf.String()
	// Compact param signatures should appear.
	if !strings.Contains(output, "(path)") {
		t.Fatal("read_file should show param signature (path)")
	}
	if !strings.Contains(output, "(path, content)") {
		t.Fatal("write_file should show param signature (path, content)")
	}
}

func TestCatalogDetail_AllParamTypes(t *testing.T) {
	services := []LocalCatalogService{{
		ServiceID:  "test",
		DaemonName: "daemon",
		Name:       "Test Service",
		Actions: []LocalCatalogAction{{
			ID: "test_action", Name: "Test", Description: "A test action",
			Params: []LocalCatalogParam{
				{Name: "required_param", Type: "string", Required: true, Description: "A required param"},
				{Name: "optional_param", Type: "integer", Required: false, Description: "An optional param"},
			},
		}},
	}}

	provider := &mockLocalProvider{services: services}
	h := newTestSkillHandler(provider)

	var buf strings.Builder
	h.writeLocalServiceDetail(&buf, context.Background(), "local.test", "user-1")

	output := buf.String()
	if !strings.Contains(output, "required_param") {
		t.Fatal("should show required param")
	}
	if !strings.Contains(output, "optional_param") {
		t.Fatal("should show optional param")
	}
	if !strings.Contains(output, "**required**") {
		t.Fatal("should mark required params")
	}
	if !strings.Contains(output, "optional") {
		t.Fatal("should mark optional params")
	}
	if !strings.Contains(output, "string") {
		t.Fatal("should show param type")
	}
	if !strings.Contains(output, "integer") {
		t.Fatal("should show param type")
	}
}

// Verify that the catalog handles the time import (ensures test compiles
// with all necessary imports).
var _ = time.Now
