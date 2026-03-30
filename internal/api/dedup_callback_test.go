package api_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// registerCallbackSecret calls POST /api/callbacks/register and returns the secret.
func registerCallbackSecret(t *testing.T, env *testEnv, agentToken string) string {
	t.Helper()
	resp := env.do("POST", "/api/callbacks/register", agentToken, nil)
	body := mustStatus(t, resp, http.StatusOK)
	secret, ok := body["callback_secret"].(string)
	if !ok || secret == "" {
		t.Fatal("registerCallbackSecret: expected non-empty callback_secret in response")
	}
	return secret
}

// ── request_id deduplication ──────────────────────────────────────────────────

func TestGateway_Dedup_BlockedRequest(t *testing.T) {
	// A blocked request, when resubmitted with the same request_id, should return
	// the existing outcome without creating a duplicate audit entry.
	env := newTestEnv(t)
	sc := newScenario(t, env, "dedup-block")
	sc.createRestriction(t, "mock.svc", "run", "blocked by test")

	reqID := fmt.Sprintf("dedup-blk-%s", randSuffix())

	first := sc.gatewayRequest(env, reqID, "mock.svc", "run")
	if first["status"] != "blocked" {
		t.Fatalf("first request: expected blocked, got %v", first["status"])
	}

	// Resubmit same request_id — dedup path should return cached outcome.
	second := sc.gatewayRequest(env, reqID, "mock.svc", "run")
	if second["status"] != "blocked" {
		t.Errorf("dedup: expected status=blocked (from cache), got %v", second["status"])
	}
	if second["request_id"] != reqID {
		t.Errorf("dedup: request_id mismatch: got %v", second["request_id"])
	}
	if second["audit_id"] != first["audit_id"] {
		t.Errorf("dedup: audit_id should match original: got %v, want %v", second["audit_id"], first["audit_id"])
	}

	// Exactly one audit entry should exist for this request_id.
	resp := sc.session.do("GET", "/api/audit", nil)
	body := mustStatus(t, resp, http.StatusOK)
	count := 0
	for _, e := range arr(t, body, "entries") {
		if e.(map[string]any)["request_id"] == reqID {
			count++
		}
	}
	if count != 1 {
		t.Errorf("dedup: expected 1 audit entry, got %d", count)
	}
}

func TestGateway_Dedup_PendingRequest(t *testing.T) {
	// A pending (awaiting approval) request, when resubmitted, should return
	// status=pending without queueing a duplicate approval.
	env := newTestEnv(t, newMockAdapter("mock.svc", "run"))
	sc := newScenario(t, env, "dedup-pend")

	taskID := sc.createApprovedTask(t, env, "mock.svc", "run", false)

	reqID := fmt.Sprintf("dedup-pend-%s", randSuffix())

	first := sc.gatewayRequestWithTask(env, reqID, "mock.svc", "run", taskID)
	if first["status"] != "pending" {
		t.Fatalf("first request: expected pending, got %v", first["status"])
	}

	second := sc.gatewayRequestWithTask(env, reqID, "mock.svc", "run", taskID)
	if second["status"] != "pending" {
		t.Errorf("dedup pending: expected status=pending, got %v", second["status"])
	}

	// Exactly one entry in the approvals queue (not two).
	resp := sc.session.do("GET", "/api/approvals", nil)
	apBody := mustStatus(t, resp, http.StatusOK)
	count := 0
	for _, e := range arr(t, apBody, "entries") {
		if e.(map[string]any)["request_id"] == reqID {
			count++
		}
	}
	if count != 1 {
		t.Errorf("dedup pending: expected 1 approval entry, got %d", count)
	}
}

func TestGateway_Dedup_ExecutedRequest(t *testing.T) {
	// An already-executed request, when resubmitted with the same request_id,
	// should return status=executed from the audit log without re-running the adapter.
	adapter := newMockAdapter("mock.dedup-exec", "run").withResult("dedup-exec-ok", nil)
	env := newTestEnv(t, adapter)
	sc := newScenario(t, env, "dedup-exec")
	if err := env.Vault.Set(context.Background(), sc.session.UserID, "mock.dedup-exec", []byte("cred")); err != nil {
		t.Fatalf("vault seed: %v", err)
	}

	taskID := sc.createApprovedTask(t, env, "mock.dedup-exec", "run", true)

	reqID := fmt.Sprintf("dedup-exec-%s", randSuffix())

	first := sc.gatewayRequestWithTask(env, reqID, "mock.dedup-exec", "run", taskID)
	if first["status"] != "executed" {
		t.Fatalf("first request: expected executed, got %v", first["status"])
	}

	second := sc.gatewayRequestWithTask(env, reqID, "mock.dedup-exec", "run", taskID)
	if second["status"] != "executed" {
		t.Errorf("dedup executed: expected status=executed, got %v", second["status"])
	}
	// The dedup path returns the same audit entry, so audit_id must match.
	if second["audit_id"] != first["audit_id"] {
		t.Errorf("dedup executed: audit_id should match original: got %v, want %v",
			second["audit_id"], first["audit_id"])
	}
}

func TestGateway_Dedup_DifferentRequestIDs_NotDeduplicated(t *testing.T) {
	// Two requests with different request_ids should each be processed independently.
	env := newTestEnv(t)
	sc := newScenario(t, env, "dedup-diff")
	sc.createRestriction(t, "mock.svc", "run", "blocked by test")

	r1 := sc.gatewayRequest(env, fmt.Sprintf("dedup-diff-a-%s", randSuffix()), "mock.svc", "run")
	r2 := sc.gatewayRequest(env, fmt.Sprintf("dedup-diff-b-%s", randSuffix()), "mock.svc", "run")

	if r1["audit_id"] == r2["audit_id"] {
		t.Error("different request_ids should produce different audit entries")
	}
}

// ── GET /api/gateway/request/{request_id} ────────────────────────────────────

func TestGateway_Status_NotFound(t *testing.T) {
	env := newTestEnv(t)
	sc := newScenario(t, env, "status-notfound")

	resp := env.do("GET", "/api/gateway/request/nonexistent-id", sc.AgentToken, nil)
	mustStatus(t, resp, http.StatusNotFound)
}

func TestGateway_Status_RequiresAgentToken(t *testing.T) {
	env := newTestEnv(t)

	resp := env.do("GET", "/api/gateway/request/some-id", "", nil)
	mustStatus(t, resp, http.StatusUnauthorized)
}

func TestGateway_Status_Blocked(t *testing.T) {
	env := newTestEnv(t)
	sc := newScenario(t, env, "status-blk")
	sc.createRestriction(t, "mock.svc", "run", "blocked by test")

	reqID := fmt.Sprintf("status-blk-%s", randSuffix())
	sc.gatewayRequest(env, reqID, "mock.svc", "run")

	resp := env.do("GET", fmt.Sprintf("/api/gateway/request/%s", reqID), sc.AgentToken, nil)
	body := mustStatus(t, resp, http.StatusOK)
	if body["status"] != "blocked" {
		t.Errorf("status: expected blocked, got %v", body["status"])
	}
	if body["request_id"] != reqID {
		t.Errorf("status: request_id mismatch: got %v", body["request_id"])
	}
	if body["audit_id"] == nil || body["audit_id"] == "" {
		t.Error("status: audit_id missing")
	}
}

func TestGateway_Status_Pending(t *testing.T) {
	env := newTestEnv(t, newMockAdapter("mock.svc", "run"))
	sc := newScenario(t, env, "status-pend")

	taskID := sc.createApprovedTask(t, env, "mock.svc", "run", false)

	reqID := fmt.Sprintf("status-pend-%s", randSuffix())
	sc.gatewayRequestWithTask(env, reqID, "mock.svc", "run", taskID)

	resp := env.do("GET", fmt.Sprintf("/api/gateway/request/%s", reqID), sc.AgentToken, nil)
	body := mustStatus(t, resp, http.StatusOK)
	if body["status"] != "pending" {
		t.Errorf("status: expected pending, got %v", body["status"])
	}
}

func TestGateway_Status_Executed(t *testing.T) {
	adapter := newMockAdapter("mock.status-exec", "run").withResult("status-exec-ok", nil)
	env := newTestEnv(t, adapter)
	sc := newScenario(t, env, "status-exec")
	if err := env.Vault.Set(context.Background(), sc.session.UserID, "mock.status-exec", []byte("cred")); err != nil {
		t.Fatalf("vault seed: %v", err)
	}

	taskID := sc.createApprovedTask(t, env, "mock.status-exec", "run", true)

	reqID := fmt.Sprintf("status-exec-%s", randSuffix())
	sc.gatewayRequestWithTask(env, reqID, "mock.status-exec", "run", taskID)

	resp := env.do("GET", fmt.Sprintf("/api/gateway/request/%s", reqID), sc.AgentToken, nil)
	body := mustStatus(t, resp, http.StatusOK)
	if body["status"] != "executed" {
		t.Errorf("status: expected executed, got %v", body["status"])
	}
}

func TestGateway_Status_UpdatesAfterDeny(t *testing.T) {
	// A pending request, once denied, should show status=denied on the status endpoint.
	env := newTestEnv(t, newMockAdapter("mock.svc", "run"))
	sc := newScenario(t, env, "status-deny")

	taskID := sc.createApprovedTask(t, env, "mock.svc", "run", false)

	reqID := fmt.Sprintf("status-deny-%s", randSuffix())
	sc.gatewayRequestWithTask(env, reqID, "mock.svc", "run", taskID)

	sc.session.do("POST", fmt.Sprintf("/api/approvals/%s/deny", reqID), nil)

	resp := env.do("GET", fmt.Sprintf("/api/gateway/request/%s", reqID), sc.AgentToken, nil)
	body := mustStatus(t, resp, http.StatusOK)
	if body["status"] != "denied" {
		t.Errorf("status after deny: expected denied, got %v", body["status"])
	}
}

func TestGateway_Status_IsolatedByUser(t *testing.T) {
	// An agent belonging to user2 cannot poll the status of user1's request.
	env := newTestEnv(t)
	sc1 := newScenario(t, env, "status-iso1")
	sc1.createRestriction(t, "mock.svc", "run", "blocked for isolation test")

	reqID := fmt.Sprintf("status-iso-%s", randSuffix())
	sc1.gatewayRequest(env, reqID, "mock.svc", "run")

	sc2 := newScenario(t, env, "status-iso2")
	resp := env.do("GET", fmt.Sprintf("/api/gateway/request/%s", reqID), sc2.AgentToken, nil)
	mustStatus(t, resp, http.StatusNotFound)
}

// ── Callback HMAC signing ─────────────────────────────────────────────────────

// callbackCapture records one received callback.
type callbackCapture struct {
	body []byte
	sig  string
}

// newCallbackServer starts a test HTTP server that records the first received POST.
func newCallbackServer(t *testing.T) (*httptest.Server, chan callbackCapture) {
	t.Helper()
	ch := make(chan callbackCapture, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		ch <- callbackCapture{body: body, sig: r.Header.Get("X-Clawvisor-Signature")}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv, ch
}

// verifyCallbackHMAC asserts that sig == "sha256=" + HMAC-SHA256(body, key).
func verifyCallbackHMAC(t *testing.T, body []byte, sig, key, label string) {
	t.Helper()
	if sig == "" {
		t.Errorf("%s: X-Clawvisor-Signature header missing", label)
		return
	}
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write(body)
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if sig != want {
		t.Errorf("%s: signature mismatch\n  got:  %s\n  want: %s", label, sig, want)
	}
}

func TestGateway_Callback_HMACSigned_OnExecute(t *testing.T) {
	// When a request is immediately executed via task, the callback POST must carry a valid
	// X-Clawvisor-Signature header, signed with the agent's registered callback secret.
	adapter := newMockAdapter("mock.cb-exec", "run").withResult("cb-exec-ok", nil)
	env := newTestEnv(t, adapter)
	sc := newScenario(t, env, "cb-exec")
	if err := env.Vault.Set(context.Background(), sc.session.UserID, "mock.cb-exec", []byte("cred")); err != nil {
		t.Fatalf("vault seed: %v", err)
	}

	// Register callback secret
	cbSecret := registerCallbackSecret(t, env, sc.AgentToken)

	taskID := sc.createApprovedTask(t, env, "mock.cb-exec", "run", true)

	cbSrv, cbCh := newCallbackServer(t)
	reqID := fmt.Sprintf("cb-exec-%s", randSuffix())

	resp := env.do("POST", "/api/gateway/request", sc.AgentToken, map[string]any{
		"service":    "mock.cb-exec",
		"action":     "run",
		"params":     map[string]any{},
		"reason":     "test callback",
		"request_id": reqID,
		"task_id":    taskID,
		"context":    map[string]any{"callback_url": cbSrv.URL + "/inbound"},
	})
	body := mustStatus(t, resp, http.StatusOK)
	if body["status"] != "executed" {
		t.Fatalf("expected executed, got %v", body["status"])
	}

	select {
	case cb := <-cbCh:
		verifyCallbackHMAC(t, cb.body, cb.sig, cbSecret, "execute callback")

		var payload map[string]any
		if err := json.Unmarshal(cb.body, &payload); err != nil {
			t.Fatalf("callback body not JSON: %v", err)
		}
		if payload["type"] != "request" {
			t.Errorf("callback: expected type=request, got %v", payload["type"])
		}
		if payload["request_id"] != reqID {
			t.Errorf("callback: request_id mismatch: got %v", payload["request_id"])
		}
		if payload["status"] != "executed" {
			t.Errorf("callback: expected status=executed, got %v", payload["status"])
		}
	case <-time.After(3 * time.Second):
		t.Fatal("execute callback not received within 3s")
	}
}

func TestApprovals_Approve_CallbackHMACSigned(t *testing.T) {
	// When a pending request is approved, the callback must be HMAC-signed with
	// the agent's registered callback secret.
	adapter := newMockAdapter("mock.cb-approve", "run").withResult("approve-cb-ok", nil)
	env := newTestEnv(t, adapter)
	sc := newScenario(t, env, "cb-approve")

	taskID := sc.createApprovedTask(t, env, "mock.cb-approve", "run", false)

	// Register callback secret
	cbSecret := registerCallbackSecret(t, env, sc.AgentToken)

	cbSrv, cbCh := newCallbackServer(t)
	reqID := fmt.Sprintf("cb-approve-%s", randSuffix())

	// Submit → pending (task has auto_execute=false)
	resp := env.do("POST", "/api/gateway/request", sc.AgentToken, map[string]any{
		"service":    "mock.cb-approve",
		"action":     "run",
		"params":     map[string]any{},
		"reason":     "test callback approve",
		"request_id": reqID,
		"task_id":    taskID,
		"context":    map[string]any{"callback_url": cbSrv.URL + "/inbound"},
	})
	firstBody := mustStatus(t, resp, http.StatusAccepted)
	if firstBody["status"] != "pending" {
		t.Fatalf("expected pending, got %v", firstBody["status"])
	}

	// Approve → triggers async callback
	resp = sc.session.do("POST", fmt.Sprintf("/api/approvals/%s/approve", reqID), nil)
	mustStatus(t, resp, http.StatusOK)

	select {
	case cb := <-cbCh:
		verifyCallbackHMAC(t, cb.body, cb.sig, cbSecret, "approve callback")

		var payload map[string]any
		if err := json.Unmarshal(cb.body, &payload); err != nil {
			t.Fatalf("callback body not JSON: %v", err)
		}
		if payload["type"] != "request" {
			t.Errorf("approve callback: expected type=request, got %v", payload["type"])
		}
		if payload["status"] != "approved" {
			t.Errorf("approve callback: expected status=approved, got %v", payload["status"])
		}
	case <-time.After(3 * time.Second):
		t.Fatal("approve callback not received within 3s")
	}
}

func TestApprovals_Deny_CallbackHMACSigned(t *testing.T) {
	// When a pending request is denied, the callback must be HMAC-signed.
	env := newTestEnv(t, newMockAdapter("mock.cb-deny", "run"))
	sc := newScenario(t, env, "cb-deny")

	taskID := sc.createApprovedTask(t, env, "mock.cb-deny", "run", false)

	// Register callback secret
	cbSecret := registerCallbackSecret(t, env, sc.AgentToken)

	cbSrv, cbCh := newCallbackServer(t)
	reqID := fmt.Sprintf("cb-deny-%s", randSuffix())

	// Submit → pending (task has auto_execute=false)
	resp := env.do("POST", "/api/gateway/request", sc.AgentToken, map[string]any{
		"service":    "mock.cb-deny",
		"action":     "run",
		"params":     map[string]any{},
		"reason":     "test callback deny",
		"request_id": reqID,
		"task_id":    taskID,
		"context":    map[string]any{"callback_url": cbSrv.URL + "/inbound"},
	})
	firstBody := mustStatus(t, resp, http.StatusAccepted)
	if firstBody["status"] != "pending" {
		t.Fatalf("expected pending, got %v", firstBody["status"])
	}

	// Deny → triggers async callback
	resp = sc.session.do("POST", fmt.Sprintf("/api/approvals/%s/deny", reqID), nil)
	mustStatus(t, resp, http.StatusOK)

	select {
	case cb := <-cbCh:
		verifyCallbackHMAC(t, cb.body, cb.sig, cbSecret, "deny callback")

		var payload map[string]any
		if err := json.Unmarshal(cb.body, &payload); err != nil {
			t.Fatalf("callback body not JSON: %v", err)
		}
		if payload["type"] != "request" {
			t.Errorf("deny callback: expected type=request, got %v", payload["type"])
		}
		if payload["status"] != "denied" {
			t.Errorf("deny callback: expected status=denied, got %v", payload["status"])
		}
	case <-time.After(3 * time.Second):
		t.Fatal("deny callback not received within 3s")
	}
}

func TestGateway_Callback_NoCallbackURL_NoDelivery(t *testing.T) {
	// Requests without a callback_url should execute normally without any callback delivery.
	adapter := newMockAdapter("mock.cb-none", "run").withResult("no-cb-ok", nil)
	env := newTestEnv(t, adapter)
	sc := newScenario(t, env, "cb-none")
	if err := env.Vault.Set(context.Background(), sc.session.UserID, "mock.cb-none", []byte("cred")); err != nil {
		t.Fatalf("vault seed: %v", err)
	}

	taskID := sc.createApprovedTask(t, env, "mock.cb-none", "run", true)

	result := sc.gatewayRequestWithTask(env, fmt.Sprintf("cb-none-%s", randSuffix()), "mock.cb-none", "run", taskID)
	if result["status"] != "executed" {
		t.Errorf("no callback_url: expected status=executed, got %v", result["status"])
	}
}

func TestGateway_Callback_Unsigned_WhenNoSecret(t *testing.T) {
	// When an agent has NOT registered a callback secret, the callback should
	// still be delivered but with an empty X-Clawvisor-Signature header.
	adapter := newMockAdapter("mock.cb-nosec", "run").withResult("nosec-ok", nil)
	env := newTestEnv(t, adapter)
	sc := newScenario(t, env, "cb-nosec")
	if err := env.Vault.Set(context.Background(), sc.session.UserID, "mock.cb-nosec", []byte("cred")); err != nil {
		t.Fatalf("vault seed: %v", err)
	}

	// Do NOT register a callback secret

	taskID := sc.createApprovedTask(t, env, "mock.cb-nosec", "run", true)

	cbSrv, cbCh := newCallbackServer(t)
	reqID := fmt.Sprintf("cb-nosec-%s", randSuffix())

	resp := env.do("POST", "/api/gateway/request", sc.AgentToken, map[string]any{
		"service":    "mock.cb-nosec",
		"action":     "run",
		"params":     map[string]any{},
		"reason":     "test unsigned callback",
		"request_id": reqID,
		"task_id":    taskID,
		"context":    map[string]any{"callback_url": cbSrv.URL + "/inbound"},
	})
	body := mustStatus(t, resp, http.StatusOK)
	if body["status"] != "executed" {
		t.Fatalf("expected executed, got %v", body["status"])
	}

	select {
	case cb := <-cbCh:
		// Callback should arrive but signature should be empty (unsigned)
		if cb.sig != "" {
			t.Errorf("unsigned callback: expected empty signature, got %q", cb.sig)
		}

		var payload map[string]any
		if err := json.Unmarshal(cb.body, &payload); err != nil {
			t.Fatalf("callback body not JSON: %v", err)
		}
		if payload["type"] != "request" {
			t.Errorf("callback: expected type=request, got %v", payload["type"])
		}
		if payload["request_id"] != reqID {
			t.Errorf("callback: request_id mismatch: got %v", payload["request_id"])
		}
	case <-time.After(3 * time.Second):
		t.Fatal("unsigned callback not received within 3s")
	}
}

func TestCallbackSecret_Register(t *testing.T) {
	// POST /api/callbacks/register should return a cbsec_-prefixed secret.
	env := newTestEnv(t)
	sc := newScenario(t, env, "cbsec-reg")

	secret := registerCallbackSecret(t, env, sc.AgentToken)
	if len(secret) < 10 || secret[:6] != "cbsec_" {
		t.Errorf("expected cbsec_-prefixed secret, got %q", secret)
	}

	// Calling again should return a different secret (rotation).
	secret2 := registerCallbackSecret(t, env, sc.AgentToken)
	if secret2 == secret {
		t.Error("expected rotated secret to differ from original")
	}
}

func TestCallbackSecret_RequiresAuth(t *testing.T) {
	env := newTestEnv(t)
	resp := env.do("POST", "/api/callbacks/register", "", nil)
	mustStatus(t, resp, http.StatusUnauthorized)
}
