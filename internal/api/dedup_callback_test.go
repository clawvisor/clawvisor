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

// agentCallbackKey derives the callback signing key from a raw agent bearer token.
// Mirrors what the gateway stores in the pending_approvals blob:
//
//	sha256(rawToken) hex-encoded
//
// This is the same hash the middleware computes to look up the agent in the DB,
// so it leaks no additional information if the blob is read.
func agentCallbackKey(rawToken string) string {
	h := sha256.Sum256([]byte(rawToken))
	return hex.EncodeToString(h[:])
}

// ── request_id deduplication ──────────────────────────────────────────────────

func TestGateway_Dedup_BlockedRequest(t *testing.T) {
	// A blocked request, when resubmitted with the same request_id, should return
	// the existing outcome without creating a duplicate audit entry.
	env := newTestEnv(t)
	sc := newScenario(t, env, "dedup-block")
	sc.createPolicy(t, blockPolicy("p-dedup-blk", "dedup-block", "mock.svc", "run"))

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
	if err := env.Vault.Set(context.Background(), sc.session.UserID, "mock.svc", []byte("dummy")); err != nil {
		t.Fatalf("vault seed: %v", err)
	}
	sc.createPolicy(t, approvePolicy("p-dedup-pend", "dedup-pend", "mock.svc", "run"))

	reqID := fmt.Sprintf("dedup-pend-%s", randSuffix())

	first := sc.gatewayRequest(env, reqID, "mock.svc", "run")
	if first["status"] != "pending" {
		t.Fatalf("first request: expected pending, got %v", first["status"])
	}

	second := sc.gatewayRequest(env, reqID, "mock.svc", "run")
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
	sc.createPolicy(t, executePolicy("p-dedup-exec", "dedup-exec", "mock.dedup-exec", "run"))
	if err := env.Vault.Set(context.Background(), sc.session.UserID, "mock.dedup-exec", []byte("cred")); err != nil {
		t.Fatalf("vault seed: %v", err)
	}

	reqID := fmt.Sprintf("dedup-exec-%s", randSuffix())

	first := sc.gatewayRequest(env, reqID, "mock.dedup-exec", "run")
	if first["status"] != "executed" {
		t.Fatalf("first request: expected executed, got %v", first["status"])
	}

	second := sc.gatewayRequest(env, reqID, "mock.dedup-exec", "run")
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
	sc.createPolicy(t, blockPolicy("p-dedup-diff", "dedup-diff", "mock.svc", "run"))

	r1 := sc.gatewayRequest(env, fmt.Sprintf("dedup-diff-a-%s", randSuffix()), "mock.svc", "run")
	r2 := sc.gatewayRequest(env, fmt.Sprintf("dedup-diff-b-%s", randSuffix()), "mock.svc", "run")

	if r1["audit_id"] == r2["audit_id"] {
		t.Error("different request_ids should produce different audit entries")
	}
}

// ── GET /api/gateway/request/{request_id}/status ─────────────────────────────

func TestGateway_Status_NotFound(t *testing.T) {
	env := newTestEnv(t)
	sc := newScenario(t, env, "status-notfound")

	resp := env.do("GET", "/api/gateway/request/nonexistent-id/status", sc.AgentToken, nil)
	mustStatus(t, resp, http.StatusNotFound)
}

func TestGateway_Status_RequiresAgentToken(t *testing.T) {
	env := newTestEnv(t)

	resp := env.do("GET", "/api/gateway/request/some-id/status", "", nil)
	mustStatus(t, resp, http.StatusUnauthorized)
}

func TestGateway_Status_Blocked(t *testing.T) {
	env := newTestEnv(t)
	sc := newScenario(t, env, "status-blk")
	sc.createPolicy(t, blockPolicy("p-status-blk", "status-blk", "mock.svc", "run"))

	reqID := fmt.Sprintf("status-blk-%s", randSuffix())
	sc.gatewayRequest(env, reqID, "mock.svc", "run")

	resp := env.do("GET", fmt.Sprintf("/api/gateway/request/%s/status", reqID), sc.AgentToken, nil)
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
	if err := env.Vault.Set(context.Background(), sc.session.UserID, "mock.svc", []byte("dummy")); err != nil {
		t.Fatalf("vault seed: %v", err)
	}
	sc.createPolicy(t, approvePolicy("p-status-pend", "status-pend", "mock.svc", "run"))

	reqID := fmt.Sprintf("status-pend-%s", randSuffix())
	sc.gatewayRequest(env, reqID, "mock.svc", "run")

	resp := env.do("GET", fmt.Sprintf("/api/gateway/request/%s/status", reqID), sc.AgentToken, nil)
	body := mustStatus(t, resp, http.StatusOK)
	if body["status"] != "pending" {
		t.Errorf("status: expected pending, got %v", body["status"])
	}
}

func TestGateway_Status_Executed(t *testing.T) {
	adapter := newMockAdapter("mock.status-exec", "run").withResult("status-exec-ok", nil)
	env := newTestEnv(t, adapter)
	sc := newScenario(t, env, "status-exec")
	sc.createPolicy(t, executePolicy("p-status-exec", "status-exec", "mock.status-exec", "run"))
	if err := env.Vault.Set(context.Background(), sc.session.UserID, "mock.status-exec", []byte("cred")); err != nil {
		t.Fatalf("vault seed: %v", err)
	}

	reqID := fmt.Sprintf("status-exec-%s", randSuffix())
	sc.gatewayRequest(env, reqID, "mock.status-exec", "run")

	resp := env.do("GET", fmt.Sprintf("/api/gateway/request/%s/status", reqID), sc.AgentToken, nil)
	body := mustStatus(t, resp, http.StatusOK)
	if body["status"] != "executed" {
		t.Errorf("status: expected executed, got %v", body["status"])
	}
}

func TestGateway_Status_UpdatesAfterDeny(t *testing.T) {
	// A pending request, once denied, should show status=denied on the status endpoint.
	env := newTestEnv(t, newMockAdapter("mock.svc", "run"))
	sc := newScenario(t, env, "status-deny")
	if err := env.Vault.Set(context.Background(), sc.session.UserID, "mock.svc", []byte("dummy")); err != nil {
		t.Fatalf("vault seed: %v", err)
	}
	sc.createPolicy(t, approvePolicy("p-status-deny", "status-deny", "mock.svc", "run"))

	reqID := fmt.Sprintf("status-deny-%s", randSuffix())
	sc.gatewayRequest(env, reqID, "mock.svc", "run")

	sc.session.do("POST", fmt.Sprintf("/api/approvals/%s/deny", reqID), nil)

	resp := env.do("GET", fmt.Sprintf("/api/gateway/request/%s/status", reqID), sc.AgentToken, nil)
	body := mustStatus(t, resp, http.StatusOK)
	if body["status"] != "denied" {
		t.Errorf("status after deny: expected denied, got %v", body["status"])
	}
}

func TestGateway_Status_IsolatedByUser(t *testing.T) {
	// An agent belonging to user2 cannot poll the status of user1's request.
	env := newTestEnv(t)
	sc1 := newScenario(t, env, "status-iso1")
	sc1.createPolicy(t, blockPolicy("p-status-iso", "status-iso1", "mock.svc", "run"))

	reqID := fmt.Sprintf("status-iso-%s", randSuffix())
	sc1.gatewayRequest(env, reqID, "mock.svc", "run")

	sc2 := newScenario(t, env, "status-iso2")
	resp := env.do("GET", fmt.Sprintf("/api/gateway/request/%s/status", reqID), sc2.AgentToken, nil)
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
	// When a request is immediately executed, the callback POST must carry a valid
	// X-Clawvisor-Signature header, signed with the agent's bearer token.
	adapter := newMockAdapter("mock.cb-exec", "run").withResult("cb-exec-ok", nil)
	env := newTestEnv(t, adapter)
	sc := newScenario(t, env, "cb-exec")
	sc.createPolicy(t, executePolicy("p-cb-exec", "cb-exec", "mock.cb-exec", "run"))
	if err := env.Vault.Set(context.Background(), sc.session.UserID, "mock.cb-exec", []byte("cred")); err != nil {
		t.Fatalf("vault seed: %v", err)
	}

	cbSrv, cbCh := newCallbackServer(t)
	reqID := fmt.Sprintf("cb-exec-%s", randSuffix())

	resp := env.do("POST", "/api/gateway/request", sc.AgentToken, map[string]any{
		"service":    "mock.cb-exec",
		"action":     "run",
		"params":     map[string]any{},
		"request_id": reqID,
		"context":    map[string]any{"callback_url": cbSrv.URL + "/inbound"},
	})
	body := mustStatus(t, resp, http.StatusOK)
	if body["status"] != "executed" {
		t.Fatalf("expected executed, got %v", body["status"])
	}

	select {
	case cb := <-cbCh:
		verifyCallbackHMAC(t, cb.body, cb.sig, agentCallbackKey(sc.AgentToken), "execute callback")

		var payload map[string]any
		if err := json.Unmarshal(cb.body, &payload); err != nil {
			t.Fatalf("callback body not JSON: %v", err)
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
	// the original agent's bearer token.
	adapter := newMockAdapter("mock.cb-approve", "run").withResult("approve-cb-ok", nil)
	env := newTestEnv(t, adapter)
	sc := newScenario(t, env, "cb-approve")
	sc.createPolicy(t, approvePolicy("p-cb-approve", "cb-approve", "mock.cb-approve", "run"))
	if err := env.Vault.Set(context.Background(), sc.session.UserID, "mock.cb-approve", []byte("cred")); err != nil {
		t.Fatalf("vault seed: %v", err)
	}

	cbSrv, cbCh := newCallbackServer(t)
	reqID := fmt.Sprintf("cb-approve-%s", randSuffix())

	// Submit → pending
	resp := env.do("POST", "/api/gateway/request", sc.AgentToken, map[string]any{
		"service":    "mock.cb-approve",
		"action":     "run",
		"params":     map[string]any{},
		"request_id": reqID,
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
		verifyCallbackHMAC(t, cb.body, cb.sig, agentCallbackKey(sc.AgentToken), "approve callback")

		var payload map[string]any
		if err := json.Unmarshal(cb.body, &payload); err != nil {
			t.Fatalf("callback body not JSON: %v", err)
		}
		if payload["status"] != "executed" {
			t.Errorf("approve callback: expected status=executed, got %v", payload["status"])
		}
	case <-time.After(3 * time.Second):
		t.Fatal("approve callback not received within 3s")
	}
}

func TestApprovals_Deny_CallbackHMACSigned(t *testing.T) {
	// When a pending request is denied, the callback must be HMAC-signed.
	env := newTestEnv(t, newMockAdapter("mock.svc", "run"))
	sc := newScenario(t, env, "cb-deny")
	if err := env.Vault.Set(context.Background(), sc.session.UserID, "mock.svc", []byte("dummy")); err != nil {
		t.Fatalf("vault seed: %v", err)
	}
	sc.createPolicy(t, approvePolicy("p-cb-deny", "cb-deny", "mock.svc", "run"))

	cbSrv, cbCh := newCallbackServer(t)
	reqID := fmt.Sprintf("cb-deny-%s", randSuffix())

	// Submit → pending
	resp := env.do("POST", "/api/gateway/request", sc.AgentToken, map[string]any{
		"service":    "mock.svc",
		"action":     "run",
		"params":     map[string]any{},
		"request_id": reqID,
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
		verifyCallbackHMAC(t, cb.body, cb.sig, agentCallbackKey(sc.AgentToken), "deny callback")

		var payload map[string]any
		if err := json.Unmarshal(cb.body, &payload); err != nil {
			t.Fatalf("callback body not JSON: %v", err)
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
	sc.createPolicy(t, executePolicy("p-cb-none", "cb-none", "mock.cb-none", "run"))
	if err := env.Vault.Set(context.Background(), sc.session.UserID, "mock.cb-none", []byte("cred")); err != nil {
		t.Fatalf("vault seed: %v", err)
	}

	result := sc.gatewayRequest(env, fmt.Sprintf("cb-none-%s", randSuffix()), "mock.cb-none", "run")
	if result["status"] != "executed" {
		t.Errorf("no callback_url: expected status=executed, got %v", result["status"])
	}
}
