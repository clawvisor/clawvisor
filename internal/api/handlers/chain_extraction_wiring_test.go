package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/clawvisor/clawvisor/internal/intent"
	"github.com/clawvisor/clawvisor/pkg/adapters"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// recordingExtractor records calls to ExtractBuiltins and ExtractLLM so tests
// can assert which extraction phases ran. Each phase returns a pre-configured
// fact stamped with the request's TaskID/SessionID so the store assertions
// reflect what would actually land in chain_facts.
type recordingExtractor struct {
	mu              sync.Mutex
	builtinCalls    int
	llmCalls        int
	builtinFactType string
	builtinValue    string
	llmFactType     string
	llmValue        string
	// done is closed when ExtractBuiltins is called for the first time so
	// tests can synchronize on the async goroutine without sleeping.
	done chan struct{}
}

func newRecordingExtractor() *recordingExtractor {
	return &recordingExtractor{
		builtinFactType: "event_id",
		builtinValue:    "evt_4o1mhcvmiq8mlhqg61bn6fpphs",
		llmFactType:     "title",
		llmValue:        "Q3 planning",
		done:            make(chan struct{}, 1),
	}
}

func (r *recordingExtractor) ExtractBuiltins(req intent.ExtractRequest) []*store.ChainFact {
	r.mu.Lock()
	r.builtinCalls++
	r.mu.Unlock()
	select {
	case r.done <- struct{}{}:
	default:
	}
	return []*store.ChainFact{{
		TaskID:    req.TaskID,
		SessionID: req.SessionID,
		AuditID:   req.AuditID,
		Service:   req.Service,
		Action:    req.Action,
		FactType:  r.builtinFactType,
		FactValue: r.builtinValue,
		Source:    "builtin",
	}}
}

func (r *recordingExtractor) ExtractLLM(_ context.Context, req intent.ExtractRequest, _ []*store.ChainFact) ([]*store.ChainFact, error) {
	r.mu.Lock()
	r.llmCalls++
	r.mu.Unlock()
	return []*store.ChainFact{{
		TaskID:    req.TaskID,
		SessionID: req.SessionID,
		AuditID:   req.AuditID,
		Service:   req.Service,
		Action:    req.Action,
		FactType:  r.llmFactType,
		FactValue: r.llmValue,
		Source:    "llm_direct",
	}}, nil
}

func (r *recordingExtractor) Extract(_ context.Context, _ intent.ExtractRequest) ([]*store.ChainFact, error) {
	return nil, nil
}

func (r *recordingExtractor) builtinCallCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.builtinCalls
}

func (r *recordingExtractor) llmCallCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.llmCalls
}

// waitForExtraction polls until ExtractBuiltins has been called at least once
// or the deadline elapses. Extraction runs in a goroutine after the response
// is written, so tests must give it a brief window.
func (r *recordingExtractor) waitForExtraction(t *testing.T) {
	t.Helper()
	select {
	case <-r.done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for ExtractBuiltins to be called")
	}
	// Give the goroutine a brief moment to also call ExtractLLM + save.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		llm := r.llmCalls
		r.mu.Unlock()
		if llm > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Allow saves to flush even when no LLM call happened.
	time.Sleep(50 * time.Millisecond)
}

func newGatewayHandlerWithRecordingExtractor(
	st *localTestStore,
	provider *mockLocalProvider,
	executor *mockLocalExecutor,
	verifier *mockVerifier,
	extractor intent.Extractor,
) *GatewayHandler {
	h := newTestGatewayHandler(st, provider, executor, verifier)
	h.extractor = extractor
	return h
}

// TestChainExtraction_Gateway_CreateAction_RunsBuiltinPhase reproduces the
// reported bug: when the verifier returns extract_context=false (the prompt
// instructs this for "create" actions), the gateway currently gates the
// entire two-phase extraction block on that flag, so the newly-created
// entity's ID never lands in chain_facts. The fix must always run the cheap
// Phase 1 builtin pass; only Phase 2 (LLM) should be gated.
func TestChainExtraction_Gateway_CreateAction_RunsBuiltinPhase(t *testing.T) {
	st := newLocalTestStore()
	st.tasks["task-1"] = &store.Task{
		ID: "task-1", UserID: "user-1", AgentID: "agent-1", Status: "active",
		AuthorizedActions: []store.TaskAction{
			{Service: "local.files", Action: "write_file", AutoExecute: true},
		},
	}

	provider := &mockLocalProvider{services: testServices()}
	executor := &mockLocalExecutor{result: &adapters.Result{
		Summary: "created",
		Data:    map[string]any{"id": "evt_4o1mhcvmiq8mlhqg61bn6fpphs"},
	}}
	// Verifier mirrors what the LLM emits for a "create" verdict per
	// prompts.go:67 — allow, but extract_context: false.
	verifier := &mockVerifier{
		verdict: &intent.VerificationVerdict{
			Allow: true, ParamScope: "ok", ReasonCoherence: "ok", ExtractContext: false,
		},
	}
	extractor := newRecordingExtractor()

	h := newGatewayHandlerWithRecordingExtractor(st, provider, executor, verifier, extractor)

	w := makeGatewayRequest(t, h, map[string]any{
		"service": "local.files",
		"action":  "write_file",
		"reason":  "create the agenda file the user asked for",
		"task_id": "task-1",
		"params":  map[string]any{"path": "/tmp/agenda.txt", "content": "..."},
	})

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	extractor.waitForExtraction(t)

	if extractor.builtinCallCount() != 1 {
		t.Errorf("ExtractBuiltins called %d times, want 1 (builtins must run even when extract_context=false; otherwise new IDs from creates never reach chain_facts)", extractor.builtinCallCount())
	}
	if extractor.llmCallCount() != 0 {
		t.Errorf("ExtractLLM called %d times, want 0 (LLM phase stays gated on extract_context)", extractor.llmCallCount())
	}

	// And the builtin fact must actually be persisted so the next request can
	// look it up via ListChainFacts.
	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.chainFacts) != 1 {
		t.Fatalf("chain_facts saved = %d, want 1", len(st.chainFacts))
	}
	if st.chainFacts[0].FactValue != "evt_4o1mhcvmiq8mlhqg61bn6fpphs" {
		t.Errorf("saved fact value = %q, want %q", st.chainFacts[0].FactValue, "evt_4o1mhcvmiq8mlhqg61bn6fpphs")
	}
	if st.chainFacts[0].Source != "builtin" {
		t.Errorf("saved fact source = %q, want %q", st.chainFacts[0].Source, "builtin")
	}
}

// TestChainExtraction_Gateway_ReadAction_RunsBothPhases is the sanity check
// for the unbroken path: when the verifier sets extract_context=true (the
// list/get/search case), both builtin and LLM extraction run.
func TestChainExtraction_Gateway_ReadAction_RunsBothPhases(t *testing.T) {
	st := newLocalTestStore()
	st.tasks["task-1"] = &store.Task{
		ID: "task-1", UserID: "user-1", AgentID: "agent-1", Status: "active",
		AuthorizedActions: []store.TaskAction{
			{Service: "local.files", Action: "read_file", AutoExecute: true},
		},
	}

	provider := &mockLocalProvider{services: testServices()}
	executor := &mockLocalExecutor{result: &adapters.Result{Summary: "ok"}}
	verifier := &mockVerifier{
		verdict: &intent.VerificationVerdict{
			Allow: true, ParamScope: "ok", ReasonCoherence: "ok", ExtractContext: true,
		},
	}
	extractor := newRecordingExtractor()

	h := newGatewayHandlerWithRecordingExtractor(st, provider, executor, verifier, extractor)

	w := makeGatewayRequest(t, h, map[string]any{
		"service": "local.files",
		"action":  "read_file",
		"reason":  "read the meeting notes",
		"task_id": "task-1",
		"params":  map[string]any{"path": "/tmp/notes.txt"},
	})

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	extractor.waitForExtraction(t)

	if extractor.builtinCallCount() != 1 {
		t.Errorf("ExtractBuiltins called %d times, want 1", extractor.builtinCallCount())
	}
	if extractor.llmCallCount() != 1 {
		t.Errorf("ExtractLLM called %d times, want 1", extractor.llmCallCount())
	}
}

// TestChainExtraction_Gateway_BuiltinsOnlyMode_SkipsLLM exercises the
// existing task-level opt-out: when chain_extraction_mode=builtins_only the
// LLM phase is skipped regardless of verdict.ExtractContext. This is the
// invariant that complements the create-action fix — builtins always run,
// LLM gating is the union of (verdict.ExtractContext) AND (mode != builtins_only).
func TestChainExtraction_Gateway_BuiltinsOnlyMode_SkipsLLM(t *testing.T) {
	st := newLocalTestStore()
	st.tasks["task-1"] = &store.Task{
		ID: "task-1", UserID: "user-1", AgentID: "agent-1", Status: "active",
		ChainExtractionMode: "builtins_only",
		AuthorizedActions: []store.TaskAction{
			{Service: "local.files", Action: "read_file", AutoExecute: true},
		},
	}

	provider := &mockLocalProvider{services: testServices()}
	executor := &mockLocalExecutor{result: &adapters.Result{Summary: "ok"}}
	verifier := &mockVerifier{
		verdict: &intent.VerificationVerdict{
			Allow: true, ParamScope: "ok", ReasonCoherence: "ok", ExtractContext: true,
		},
	}
	extractor := newRecordingExtractor()

	h := newGatewayHandlerWithRecordingExtractor(st, provider, executor, verifier, extractor)

	w := makeGatewayRequest(t, h, map[string]any{
		"service": "local.files",
		"action":  "read_file",
		"reason":  "read the meeting notes",
		"task_id": "task-1",
		"params":  map[string]any{"path": "/tmp/notes.txt"},
	})

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	extractor.waitForExtraction(t)

	if extractor.builtinCallCount() != 1 {
		t.Errorf("ExtractBuiltins called %d times, want 1", extractor.builtinCallCount())
	}
	if extractor.llmCallCount() != 0 {
		t.Errorf("ExtractLLM called %d times, want 0 (builtins_only)", extractor.llmCallCount())
	}
}

// ── Approval execution path ─────────────────────────────────────────────────

// approvalTestStore extends localTestStore with the pending-approval methods
// executeAndRespond exercises. Kept narrowly scoped so other tests using
// localTestStore aren't affected.
type approvalTestStore struct {
	*localTestStore

	paMu     sync.Mutex
	pa       *store.PendingApproval
	claimed  bool
	deleted  bool
}

func newApprovalTestStore() *approvalTestStore {
	return &approvalTestStore{localTestStore: newLocalTestStore()}
}

func (s *approvalTestStore) GetPendingApproval(_ context.Context, requestID, userID string) (*store.PendingApproval, error) {
	s.paMu.Lock()
	defer s.paMu.Unlock()
	if s.pa == nil || s.pa.RequestID != requestID || s.pa.UserID != userID {
		return nil, store.ErrNotFound
	}
	return s.pa, nil
}

func (s *approvalTestStore) GetPendingApprovalByTask(ctx context.Context, requestID, userID, _ string) (*store.PendingApproval, error) {
	return s.GetPendingApproval(ctx, requestID, userID)
}

func (s *approvalTestStore) ClaimPendingApprovalForExecution(_ context.Context, requestID, userID, _ string) (bool, error) {
	s.paMu.Lock()
	defer s.paMu.Unlock()
	if s.pa == nil || s.pa.RequestID != requestID || s.pa.UserID != userID {
		return false, nil
	}
	if s.claimed {
		return false, nil
	}
	s.claimed = true
	return true, nil
}

func (s *approvalTestStore) DeletePendingApproval(_ context.Context, requestID, userID, _ string) error {
	s.paMu.Lock()
	defer s.paMu.Unlock()
	if s.pa != nil && s.pa.RequestID == requestID && s.pa.UserID == userID {
		s.deleted = true
	}
	return nil
}

func makeExecuteApprovedRequest(t *testing.T, h *GatewayHandler, requestID string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/api/gateway/request/"+requestID+"/execute", nil)
	req.SetPathValue("request_id", requestID)
	req = req.WithContext(withAgent(req.Context(), testAgent))
	w := httptest.NewRecorder()
	h.HandleExecuteApproved(w, req)
	return w
}

// TestChainExtraction_ExecuteApproved_RunsBuiltinPhase covers the second
// half of the reported bug: when a "create" request goes through per-request
// approval (the typical path when auto_execute=false), executeAndRespond
// currently does no chain extraction at all — the new entity's ID never
// reaches chain_facts, so any downstream "update_*" reference is treated as
// a chain-context violation.
func TestChainExtraction_ExecuteApproved_RunsBuiltinPhase(t *testing.T) {
	st := newApprovalTestStore()
	st.tasks["task-1"] = &store.Task{
		ID: "task-1", UserID: "user-1", AgentID: "agent-1", Status: "active",
		AuthorizedActions: []store.TaskAction{
			{Service: "local.files", Action: "write_file"}, // auto_execute=false implied
		},
	}

	blob := pendingRequestBlob{
		Service:   "local.files",
		Action:    "write_file",
		Params:    map[string]any{"path": "/tmp/agenda.txt", "content": "..."},
		UserID:    "user-1",
		AgentID:   "agent-1",
		AgentName: "test-agent",
		RequestID: "req-create-1",
		TaskID:    "task-1",
		Reason:    "create the agenda file",
		// Advisory verdict stored at approval time for a "create" action:
		// allow but extract_context=false (per prompts.go:67).
		Verification: &intent.VerificationVerdict{
			Allow: true, ParamScope: "ok", ReasonCoherence: "ok", ExtractContext: false,
		},
	}
	blobBytes, _ := json.Marshal(blob)
	taskID := "task-1"
	st.pa = &store.PendingApproval{
		UserID:      "user-1",
		RequestID:   "req-create-1",
		AuditID:     "audit-1",
		TaskID:      &taskID,
		RequestBlob: blobBytes,
		Status:      "approved",
		ExpiresAt:   time.Now().Add(10 * time.Minute),
	}

	executor := &mockLocalExecutor{result: &adapters.Result{
		Summary: "created",
		Data:    map[string]any{"id": "evt_4o1mhcvmiq8mlhqg61bn6fpphs"},
	}}
	provider := &mockLocalProvider{services: testServices()}

	// Wrap approvalTestStore in something with localTestStore methods exposed —
	// newTestGatewayHandler takes *localTestStore. The embedded pointer means
	// the gateway will use approvalTestStore's overrides when called via
	// store.Store, but the test helper's signature wants *localTestStore.
	verifier := &mockVerifier{verdict: &intent.VerificationVerdict{Allow: true}}
	extractor := newRecordingExtractor()
	h := newGatewayHandlerWithApprovalStore(st, provider, executor, verifier, extractor)

	w := makeExecuteApprovedRequest(t, h, "req-create-1")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	extractor.waitForExtraction(t)

	if extractor.builtinCallCount() != 1 {
		t.Errorf("ExtractBuiltins called %d times, want 1 (post-approval create must populate chain_facts so downstream update_* requests resolve)", extractor.builtinCallCount())
	}
	if extractor.llmCallCount() != 0 {
		t.Errorf("ExtractLLM called %d times, want 0 (advisory verdict extract_context=false)", extractor.llmCallCount())
	}

	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.chainFacts) != 1 {
		t.Fatalf("chain_facts saved = %d, want 1", len(st.chainFacts))
	}
}

// newGatewayHandlerWithApprovalStore wires an approvalTestStore (which extends
// localTestStore with PendingApproval methods) into a GatewayHandler.
func newGatewayHandlerWithApprovalStore(
	st *approvalTestStore,
	provider *mockLocalProvider,
	executor *mockLocalExecutor,
	verifier *mockVerifier,
	extractor intent.Extractor,
) *GatewayHandler {
	h := newTestGatewayHandler(st.localTestStore, provider, executor, verifier)
	// Re-point the store to the wrapper so PendingApproval methods route
	// through approvalTestStore's overrides rather than panicking on the
	// embedded nil store.Store interface.
	h.store = st
	h.extractor = extractor
	return h
}
