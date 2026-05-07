package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"golang.org/x/time/rate"

	"github.com/clawvisor/clawvisor/internal/intent"
	"github.com/clawvisor/clawvisor/internal/ratelimit"
	"github.com/clawvisor/clawvisor/pkg/adapters"
	"github.com/clawvisor/clawvisor/pkg/gateway"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// ── verdictErrorCode ────────────────────────────────────────────────────────

func TestVerdictErrorCode_ReasonIncoherent(t *testing.T) {
	got := verdictErrorCode(&intent.VerificationVerdict{ReasonCoherence: "incoherent"})
	if got != gateway.CodeReasonTooVague {
		t.Fatalf("expected REASON_TOO_VAGUE for incoherent, got %q", got)
	}
}

func TestVerdictErrorCode_ReasonInsufficient(t *testing.T) {
	got := verdictErrorCode(&intent.VerificationVerdict{ReasonCoherence: "insufficient"})
	if got != gateway.CodeReasonTooVague {
		t.Fatalf("expected REASON_TOO_VAGUE for insufficient, got %q", got)
	}
}

func TestVerdictErrorCode_ParamViolation(t *testing.T) {
	got := verdictErrorCode(&intent.VerificationVerdict{
		ReasonCoherence: "ok",
		ParamScope:      "violation",
	})
	if got != gateway.CodeParamViolation {
		t.Fatalf("expected PARAM_VIOLATION, got %q", got)
	}
}

func TestVerdictErrorCode_ReasonTakesPrecedenceOverParams(t *testing.T) {
	got := verdictErrorCode(&intent.VerificationVerdict{
		ReasonCoherence: "incoherent",
		ParamScope:      "violation",
	})
	if got != gateway.CodeReasonTooVague {
		t.Fatalf("reason coherence should beat param scope; got %q", got)
	}
}

func TestVerdictErrorCode_NilFallback(t *testing.T) {
	if got := verdictErrorCode(nil); got != gateway.CodeRestricted {
		t.Fatalf("expected RESTRICTED for nil verdict, got %q", got)
	}
}

// ── Batch handler ───────────────────────────────────────────────────────────

func makeBatchRequest(t *testing.T, h *GatewayHandler, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(body)
	req := httptest.NewRequest("POST", "/api/gateway/batch", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(withAgent(req.Context(), testAgent))
	w := httptest.NewRecorder()
	h.HandleBatch(w, req)
	return w
}

func TestBatch_Empty(t *testing.T) {
	st := newLocalTestStore()
	h := newTestGatewayHandler(st, nil, nil, &mockVerifier{})

	w := makeBatchRequest(t, h, map[string]any{"requests": []any{}})

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["code"] != gateway.CodeBatchEmpty {
		t.Fatalf("expected BATCH_EMPTY, got %v", resp["code"])
	}
}

func TestBatch_TooLarge(t *testing.T) {
	st := newLocalTestStore()
	h := newTestGatewayHandler(st, nil, nil, &mockVerifier{})

	reqs := make([]map[string]any, maxBatchSize+1)
	for i := range reqs {
		reqs[i] = map[string]any{
			"service":    "local.files",
			"action":     "read_file",
			"reason":     "r",
			"task_id":    "task-1",
			"request_id": "r" + string(rune('a'+i%26)),
		}
	}
	w := makeBatchRequest(t, h, map[string]any{"requests": reqs})

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["code"] != gateway.CodeBatchTooLarge {
		t.Fatalf("expected BATCH_TOO_LARGE, got %v", resp["code"])
	}
}

// TestBatch_RateLimitChargesPerSubRequest is the regression guard for the
// "one token buys 20 sub-requests" bug. The route-level middleware already
// consumed one token for the batch envelope; HandleBatch must charge the
// remaining N-1 tokens before fanning out.
func TestBatch_RateLimitChargesPerSubRequest(t *testing.T) {
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
		verdict: &intent.VerificationVerdict{Allow: true, ParamScope: "ok", ReasonCoherence: "ok"},
	}

	h := newTestGatewayHandler(st, provider, executor, verifier)

	// Limiter with bucket=2 — enough for the route-level token and one
	// sub-request, but a 3-request batch (1 envelope + 2 extras) must be
	// rejected because charging the 2nd extra exhausts the bucket.
	limiter := ratelimit.NewKeyedLimiter(rate.Limit(0.0001), 2)
	keyFn := func(r *http.Request) string { return "agent-1" }
	h.SetGatewayRateLimiter(limiter, keyFn)

	// Drain the bucket as the route-level middleware would: take 1 for the
	// batch envelope itself.
	if allowed, _, _ := limiter.Allow("agent-1"); !allowed {
		t.Fatalf("setup: expected envelope token to be available")
	}

	reqs := make([]map[string]any, 3)
	for i := range reqs {
		reqs[i] = map[string]any{
			"service":    "local.files",
			"action":     "read_file",
			"reason":     "r",
			"task_id":    "task-1",
			"request_id": "r" + string(rune('a'+i)),
		}
	}
	w := makeBatchRequest(t, h, map[string]any{"requests": reqs})
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 when batch fan-out exceeds bucket, got %d: %s", w.Code, w.Body.String())
	}

	if executor.called {
		t.Fatal("executor must not have been called when batch was rate-limited")
	}
}

// TestBatch_RateLimitHeaderReflectsFanOut is the regression guard for the
// observability gap I shipped in the original H31 fix: the route-level
// middleware writes X-RateLimit-Remaining BEFORE HandleBatch runs, so
// without this fix the header always reported "limit-1" regardless of
// batch size — agents couldn't tell from headers that a 20-request batch
// burned 20 tokens. HandleBatch must overwrite the header after its
// internal charges so the reported value matches what was actually
// consumed.
func TestBatch_RateLimitHeaderReflectsFanOut(t *testing.T) {
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
		verdict: &intent.VerificationVerdict{Allow: true, ParamScope: "ok", ReasonCoherence: "ok"},
	}

	h := newTestGatewayHandler(st, provider, executor, verifier)

	// 60-token bucket — same defaults as production.
	limiter := ratelimit.NewKeyedLimiter(rate.Limit(1), 60)
	h.SetGatewayRateLimiter(limiter, func(*http.Request) string { return "agent-1" })

	// Drain the envelope token (mid-route middleware would do this).
	if allowed, _, _ := limiter.Allow("agent-1"); !allowed {
		t.Fatalf("setup: expected envelope token to be available")
	}

	// 5-request batch — HandleBatch should charge 4 more tokens internally.
	reqs := make([]map[string]any, 5)
	for i := range reqs {
		reqs[i] = map[string]any{
			"service":    "local.files",
			"action":     "read_file",
			"reason":     "r",
			"task_id":    "task-1",
			"request_id": "r" + string(rune('a'+i)),
		}
	}
	w := makeBatchRequest(t, h, map[string]any{"requests": reqs})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// After: envelope (1) + fan-out charges (4) = 5 consumed of 60.
	// The header must report 60 - 5 = 55 remaining, NOT 59.
	got := w.Header().Get("X-RateLimit-Remaining")
	if got != "55" {
		t.Fatalf("expected X-RateLimit-Remaining=55 after 5-request batch, got %q", got)
	}
}

func TestBatch_FanOut_MixedOutcomes(t *testing.T) {
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
			Allow: true, ParamScope: "ok", ReasonCoherence: "ok", ExtractContext: false,
		},
	}

	h := newTestGatewayHandler(st, provider, executor, verifier)

	w := makeBatchRequest(t, h, map[string]any{
		"requests": []map[string]any{
			// Sub-request 1: valid, should execute.
			{
				"service":    "local.files",
				"action":     "read_file",
				"reason":     "read config",
				"task_id":    "task-1",
				"request_id": "req-1",
				"params":     map[string]any{"path": "/etc/hosts"},
			},
			// Sub-request 2: unknown action, should fail with UNKNOWN_ACTION.
			{
				"service":    "local.files",
				"action":     "delete_file",
				"reason":     "delete something",
				"task_id":    "task-1",
				"request_id": "req-2",
			},
			// Sub-request 3: out of scope (write_file not in authorized actions).
			{
				"service":    "local.files",
				"action":     "write_file",
				"reason":     "write something",
				"task_id":    "task-1",
				"request_id": "req-3",
				"params":     map[string]any{"path": "/tmp/x", "content": "y"},
			},
		},
	})

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp gateway.BatchResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Results) != 3 {
		t.Fatalf("expected 3 results, got %d", len(resp.Results))
	}

	// Results must be aligned with input order via request_id.
	byID := map[string]map[string]any{}
	for _, r := range resp.Results {
		id, _ := r["request_id"].(string)
		byID[id] = r
	}

	r1 := byID["req-1"]
	if r1 == nil || r1["status"] != "executed" {
		t.Fatalf("req-1 expected executed, got %v", r1)
	}
	r2 := byID["req-2"]
	if r2 == nil || r2["code"] != gateway.CodeUnknownAction {
		t.Fatalf("req-2 expected UNKNOWN_ACTION, got %v", r2)
	}
	r3 := byID["req-3"]
	if r3 == nil || r3["code"] != gateway.CodeScopeMismatch {
		t.Fatalf("req-3 expected SCOPE_MISMATCH, got %v", r3)
	}
}

func TestBatch_ResultsPreserveOrder(t *testing.T) {
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
		verdict: &intent.VerificationVerdict{Allow: true, ParamScope: "ok", ReasonCoherence: "ok"},
	}
	h := newTestGatewayHandler(st, provider, executor, verifier)

	reqs := make([]map[string]any, 5)
	for i := range reqs {
		reqs[i] = map[string]any{
			"service":    "local.files",
			"action":     "read_file",
			"reason":     "read config",
			"task_id":    "task-1",
			"request_id": "req-" + string(rune('1'+i)),
			"params":     map[string]any{"path": "/etc/hosts"},
		}
	}
	w := makeBatchRequest(t, h, map[string]any{"requests": reqs})

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp gateway.BatchResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Results) != 5 {
		t.Fatalf("expected 5 results, got %d", len(resp.Results))
	}
	for i, r := range resp.Results {
		want := "req-" + string(rune('1'+i))
		if got, _ := r["request_id"].(string); got != want {
			t.Fatalf("result[%d] request_id=%q, want %q", i, got, want)
		}
	}
}

func TestBatch_SubRequestPanic_RecoveredPerGoroutine(t *testing.T) {
	// A panic in a sub-request goroutine must not take down the process.
	// It should be recovered, produce an INTERNAL_ERROR result for the
	// failing sub-request, and leave the rest of the batch intact.
	st := newLocalTestStore()
	st.tasks["task-1"] = &store.Task{
		ID: "task-1", UserID: "user-1", AgentID: "agent-1", Status: "active",
		AuthorizedActions: []store.TaskAction{
			{Service: "local.files", Action: "read_file", AutoExecute: true},
		},
	}
	provider := &mockLocalProvider{services: testServices()}
	executor := &mockLocalExecutor{result: &adapters.Result{Summary: "ok"}}
	// Panic from inside the single-request pipeline.
	verifier := &mockVerifier{panicWith: "synthetic panic for test"}

	h := newTestGatewayHandler(st, provider, executor, verifier)

	w := makeBatchRequest(t, h, map[string]any{
		"requests": []map[string]any{
			{
				"service":    "local.files",
				"action":     "read_file",
				"reason":     "read config",
				"task_id":    "task-1",
				"request_id": "req-panic",
				"params":     map[string]any{"path": "/etc/hosts"},
			},
		},
	})

	if w.Code != http.StatusOK {
		t.Fatalf("batch response must be 200 even if a sub-request panicked, got %d: %s", w.Code, w.Body.String())
	}
	var resp gateway.BatchResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(resp.Results))
	}
	r := resp.Results[0]
	if r["code"] != gateway.CodeInternalError {
		t.Fatalf("expected INTERNAL_ERROR after panic recovery, got %v", r["code"])
	}
	if r["request_id"] != "req-panic" {
		t.Fatalf("expected request_id preserved, got %v", r["request_id"])
	}
}

func TestBatch_Unauthenticated(t *testing.T) {
	st := newLocalTestStore()
	h := newTestGatewayHandler(st, nil, nil, &mockVerifier{})

	// No agent in context.
	req := httptest.NewRequest("POST", "/api/gateway/batch",
		strings.NewReader(`{"requests":[{"service":"x","action":"y","reason":"z","task_id":"t","request_id":"r"}]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.HandleBatch(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}
