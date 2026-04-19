package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/clawvisor/clawvisor/internal/api/middleware"
	"github.com/clawvisor/clawvisor/internal/auth"
	"github.com/clawvisor/clawvisor/internal/groupchat"
	sqlitestore "github.com/clawvisor/clawvisor/internal/store/sqlite"
	"github.com/clawvisor/clawvisor/pkg/store"
)

func newTestStore(t *testing.T) store.Store {
	t.Helper()
	ctx := context.Background()
	db, err := sqlitestore.New(ctx, t.TempDir()+"/test.db")
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return sqlitestore.NewStore(db)
}

func mustUser(t *testing.T, st store.Store, email string) *store.User {
	t.Helper()
	u, err := st.CreateUser(context.Background(), email, "hash")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	return u
}

func withUserContext(req *http.Request, u *store.User) *http.Request {
	return req.WithContext(context.WithValue(req.Context(), middleware.UserContextKey, u))
}

// TestPluginPair_ApproveMintsBridgeAndAgentTokens verifies the core happy
// path: user approves a pending pair → server mints a bridge token and one
// agent token per requested agent_id; the long-poll cached bundle contains
// both sets so the plugin's wait response can deliver them.
func TestPluginPair_ApproveMintsBridgeAndAgentTokens(t *testing.T) {
	st := newTestStore(t)
	user := mustUser(t, st, "alice@test.example")
	h := NewPluginPairingHandler(st, nil, slog.Default())

	// Seed a pending pair request (as if the plugin had POSTed /api/plugin/pair).
	pr := &store.PluginPairRequest{
		UserID:             user.ID,
		InstallFingerprint: "fp_test",
		Hostname:           "alice-laptop",
		AgentIDs:           []string{"main", "researcher"},
		Status:             "pending",
		ExpiresAt:          time.Now().Add(5 * time.Minute),
	}
	if err := st.CreatePluginPairRequest(context.Background(), pr); err != nil {
		t.Fatalf("CreatePluginPairRequest: %v", err)
	}

	body, _ := json.Marshal(map[string]any{"auto_approval_enabled": true})
	req := httptest.NewRequest(http.MethodPost, "/api/plugin/pair/"+pr.ID+"/approve", bytes.NewReader(body))
	req.SetPathValue("id", pr.ID)
	req = withUserContext(req, user)
	rec := httptest.NewRecorder()

	h.ApprovePair(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("approve status=%d body=%s", rec.Code, rec.Body.String())
	}

	// Pair request should be marked approved and linked to a bridge.
	got, err := st.GetPluginPairRequest(context.Background(), pr.ID)
	if err != nil {
		t.Fatalf("GetPluginPairRequest: %v", err)
	}
	if got.Status != "approved" {
		t.Fatalf("expected status=approved, got %q", got.Status)
	}
	if got.BridgeTokenID == "" {
		t.Fatal("expected bridge_token_id to be populated on approve")
	}

	// Bridge token exists with auto_approval_enabled=true.
	bt, err := st.GetBridgeTokenByID(context.Background(), got.BridgeTokenID)
	if err != nil {
		t.Fatalf("GetBridgeTokenByID: %v", err)
	}
	if !bt.AutoApprovalEnabled {
		t.Fatal("bridge token should have auto_approval_enabled=true from approve body")
	}

	// Bundle cache contains raw bridge + agent tokens for the plugin's long-poll.
	bundle, ok := h.loadBundle(pr.ID)
	if !ok {
		t.Fatal("expected token bundle to be cached after approve")
	}
	if !strings.HasPrefix(bundle.BridgeToken, "cvisbr_") {
		t.Fatalf("bridge_token should have cvisbr_ prefix, got %q", bundle.BridgeToken)
	}
	if len(bundle.Agents) != 2 {
		t.Fatalf("expected 2 agent tokens in bundle, got %d", len(bundle.Agents))
	}
	for name, token := range bundle.Agents {
		if !strings.HasPrefix(token, "cvis_") || strings.HasPrefix(token, "cvisbr_") {
			t.Fatalf("agent %q token has wrong prefix: %q", name, token)
		}
	}

	// Agent rows exist under the user.
	agents, err := st.ListAgents(context.Background(), user.ID)
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	names := map[string]bool{}
	for _, a := range agents {
		names[a.Name] = true
	}
	for _, want := range []string{"main", "researcher"} {
		if !names[want] {
			t.Fatalf("expected agent %q to be created, got %+v", want, names)
		}
	}
}

// TestPluginPair_ApproveIsAtomic confirms the approve flow does not leave
// orphan bridge / agent rows if the pair request transitions out from
// under it. Concrete scenario: the pair request is flipped to "denied"
// *between* the handler's initial read and the transactional approve
// call. The transaction must detect this and roll back — zero bridge or
// agent rows left behind.
func TestPluginPair_ApproveIsAtomic(t *testing.T) {
	st := newTestStore(t)
	user := mustUser(t, st, "atomic@test.example")
	ctx := context.Background()

	pr := &store.PluginPairRequest{
		UserID:    user.ID,
		AgentIDs:  []string{"a", "b"},
		Status:    "pending",
		ExpiresAt: time.Now().Add(5 * time.Minute),
	}
	if err := st.CreatePluginPairRequest(ctx, pr); err != nil {
		t.Fatalf("CreatePluginPairRequest: %v", err)
	}

	// Simulate a concurrent deny landing while the handler was mid-decision.
	if err := st.UpdatePluginPairRequestStatus(ctx, pr.ID, "denied", ""); err != nil {
		t.Fatalf("UpdatePluginPairRequestStatus: %v", err)
	}

	// Now call ApprovePluginPair directly — it should see the non-pending
	// state inside the tx and refuse to mint anything.
	_, err := st.ApprovePluginPair(ctx, store.ApprovePluginPairInput{
		PairRequestID: pr.ID,
		UserID:        user.ID,
		NewBridge: &store.NewBridgeInput{
			TokenHash:           "sha-test",
			InstallFingerprint:  "fp",
			Hostname:            "h",
			AutoApprovalEnabled: false,
		},
		Agents: []store.AgentMintInput{
			{Name: "a", TokenHash: "ahash"},
			{Name: "b", TokenHash: "bhash"},
		},
	})
	if err == nil {
		t.Fatal("approve should fail when pair request is no longer pending")
	}

	// Nothing leaked: no bridge tokens for this user, no agents named a/b.
	bridges, _ := st.ListActiveBridgesForUser(ctx, user.ID)
	if len(bridges) != 0 {
		t.Fatalf("tx rollback failed: bridge_tokens row leaked (%d)", len(bridges))
	}
	agents, _ := st.ListAgents(ctx, user.ID)
	if len(agents) != 0 {
		t.Fatalf("tx rollback failed: agent rows leaked (%d)", len(agents))
	}
}

// TestPluginPair_DenyLeavesNoTokens verifies that a denied pair leaves no
// bridge/agent rows and no cached bundle — critical so a denied attacker
// can't later retrieve tokens from the waiter cache.
func TestPluginPair_DenyLeavesNoTokens(t *testing.T) {
	st := newTestStore(t)
	user := mustUser(t, st, "bob@test.example")
	h := NewPluginPairingHandler(st, nil, slog.Default())

	pr := &store.PluginPairRequest{
		UserID:    user.ID,
		AgentIDs:  []string{"main"},
		Status:    "pending",
		ExpiresAt: time.Now().Add(5 * time.Minute),
	}
	_ = st.CreatePluginPairRequest(context.Background(), pr)

	req := httptest.NewRequest(http.MethodPost, "/api/plugin/pair/"+pr.ID+"/deny", nil)
	req.SetPathValue("id", pr.ID)
	req = withUserContext(req, user)
	rec := httptest.NewRecorder()
	h.DenyPair(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("deny status=%d", rec.Code)
	}
	if _, ok := h.loadBundle(pr.ID); ok {
		t.Fatal("deny should not populate the token bundle cache")
	}
	bridges, _ := st.ListActiveBridgesForUser(context.Background(), user.ID)
	if len(bridges) != 0 {
		t.Fatalf("deny should not create bridge tokens, got %d", len(bridges))
	}
}

// TestPluginPair_CrossUserApproveForbidden confirms a pair request belonging
// to user A cannot be approved by user B — otherwise any logged-in dashboard
// user could claim any pending install.
func TestPluginPair_CrossUserApproveForbidden(t *testing.T) {
	st := newTestStore(t)
	alice := mustUser(t, st, "alice@test.example")
	bob := mustUser(t, st, "bob@test.example")
	h := NewPluginPairingHandler(st, nil, slog.Default())

	pr := &store.PluginPairRequest{
		UserID:    alice.ID,
		AgentIDs:  []string{"main"},
		Status:    "pending",
		ExpiresAt: time.Now().Add(5 * time.Minute),
	}
	_ = st.CreatePluginPairRequest(context.Background(), pr)

	body, _ := json.Marshal(map[string]any{"auto_approval_enabled": false})
	req := httptest.NewRequest(http.MethodPost, "/api/plugin/pair/"+pr.ID+"/approve", bytes.NewReader(body))
	req.SetPathValue("id", pr.ID)
	req = withUserContext(req, bob) // wrong user
	rec := httptest.NewRecorder()

	h.ApprovePair(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 when wrong user approves, got %d body=%s", rec.Code, rec.Body.String())
	}
}

// TestBridgeMiddleware_RejectsAgentToken verifies the token-type split: the
// bridge middleware explicitly rejects tokens that lack the cvisbr_ prefix,
// so a stolen agent token cannot reach /api/buffer/ingest or any bridge-only
// endpoint even if the lookup would happen to succeed.
func TestBridgeMiddleware_RejectsAgentToken(t *testing.T) {
	st := newTestStore(t)
	mw := middleware.RequireBridge(st)
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodPost, "/api/buffer/ingest", nil)
	req.Header.Set("Authorization", "Bearer cvis_agent_token_abc") // wrong prefix
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for wrong-prefix token, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "WRONG_TOKEN_TYPE") {
		t.Fatalf("expected WRONG_TOKEN_TYPE code, got body=%s", rec.Body.String())
	}
}

// TestBridgeMiddleware_RejectsRevokedBridge verifies that revoke takes
// effect on the next request — a revoked bridge token cannot continue
// forwarding even if it hasn't been rotated.
func TestBridgeMiddleware_RejectsRevokedBridge(t *testing.T) {
	st := newTestStore(t)
	user := mustUser(t, st, "alice@test.example")
	raw, _ := auth.GenerateBridgeToken()
	bt := &store.BridgeToken{UserID: user.ID, TokenHash: auth.HashToken(raw)}
	_ = st.CreateBridgeToken(context.Background(), bt)
	_ = st.RevokeBridgeToken(context.Background(), bt.ID, user.ID)

	mw := middleware.RequireBridge(st)
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodPost, "/api/buffer/ingest", nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for revoked bridge, got %d body=%s", rec.Code, rec.Body.String())
	}
}

// TestAutoApproval_Gate exercises resolveAttestingBridge, the server-side
// trust boundary for OpenClaw auto-approval. The agent tool schema hides
// group_chat_id, but nothing stops a compromised or misbehaving agent from
// POSTing it directly to /api/tasks — so the real gate is the bridge
// attestation header. Without a valid attestation, the bridge reference is
// dropped and auto-approval cannot fire.
func TestAutoApproval_Gate_RequiresValidBridgeAttestation(t *testing.T) {
	st := newTestStore(t)
	alice := mustUser(t, st, "alice@test.example")
	bob := mustUser(t, st, "bob@test.example")
	h := &TasksHandler{st: st, logger: slog.Default()}
	ctx := context.Background()

	rawAlice, _ := auth.GenerateBridgeToken()
	aliceBridge := &store.BridgeToken{
		UserID:              alice.ID,
		TokenHash:           auth.HashToken(rawAlice),
		AutoApprovalEnabled: true,
	}
	_ = st.CreateBridgeToken(ctx, aliceBridge)

	rawBob, _ := auth.GenerateBridgeToken()
	bobBridge := &store.BridgeToken{
		UserID:              bob.ID,
		TokenHash:           auth.HashToken(rawBob),
		AutoApprovalEnabled: true,
	}
	_ = st.CreateBridgeToken(ctx, bobBridge)

	newReqWithHeader := func(val string) *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/api/tasks", nil)
		if val != "" {
			req.Header.Set(bridgeAttestationHeader, val)
		}
		return req
	}

	// Missing header → nil (gate closed).
	if h.resolveAttestingBridge(ctx, newReqWithHeader(""), alice.ID) != nil {
		t.Fatal("missing attestation should return nil")
	}
	// Malformed (missing Bearer prefix) → nil.
	if h.resolveAttestingBridge(ctx, newReqWithHeader(rawAlice), alice.ID) != nil {
		t.Fatal("header without Bearer prefix should return nil")
	}
	// Wrong-prefix token (cvis_ = agent) in the attestation slot → nil.
	if h.resolveAttestingBridge(ctx, newReqWithHeader("Bearer cvis_agent_token"), alice.ID) != nil {
		t.Fatal("agent-prefix token in attestation header should be rejected")
	}
	// Unknown bridge token → nil.
	if h.resolveAttestingBridge(ctx, newReqWithHeader("Bearer cvisbr_notareal_token"), alice.ID) != nil {
		t.Fatal("unknown bridge token should be rejected")
	}
	// Bob's bridge attesting for a task under Alice's agent → nil.
	// This is the exact attack the bridge binding prevents: you cannot use
	// one user's auto-approval opt-in to approve another user's tasks.
	if h.resolveAttestingBridge(ctx, newReqWithHeader("Bearer "+rawBob), alice.ID) != nil {
		t.Fatal("cross-user attestation must be rejected")
	}
	// Alice's bridge attesting for Alice's task → returned.
	got := h.resolveAttestingBridge(ctx, newReqWithHeader("Bearer "+rawAlice), alice.ID)
	if got == nil || got.ID != aliceBridge.ID {
		t.Fatalf("valid same-user attestation should return the bridge, got %+v", got)
	}

	// Revoked → rejected even with the correct token.
	_ = st.RevokeBridgeToken(ctx, aliceBridge.ID, alice.ID)
	if h.resolveAttestingBridge(ctx, newReqWithHeader("Bearer "+rawAlice), alice.ID) != nil {
		t.Fatal("revoked bridge should be rejected")
	}
}

// TestPluginPair_PairCodeFlow covers the pair-code happy path + its three
// must-hold invariants: single-use consumption, expiry enforcement, and
// idempotent-retry behavior where a second RequestPair with the same
// Idempotency-Key collapses onto the original pair request instead of
// minting a second pending card.
func TestPluginPair_PairCodeFlow(t *testing.T) {
	st := newTestStore(t)
	user := mustUser(t, st, "alice@test.example")
	h := NewPluginPairingHandler(st, nil, slog.Default())
	ctx := context.Background()

	// Mint a pair code via the handler (user JWT).
	mintReq := httptest.NewRequest(http.MethodPost, "/api/plugin/pair-codes", nil)
	mintReq = withUserContext(mintReq, user)
	mintRec := httptest.NewRecorder()
	h.MintPairCode(mintRec, mintReq)
	if mintRec.Code != http.StatusCreated {
		t.Fatalf("mint status=%d body=%s", mintRec.Code, mintRec.Body.String())
	}
	var mintResp struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(mintRec.Body).Decode(&mintResp); err != nil {
		t.Fatalf("decode mint: %v", err)
	}
	if !strings.HasPrefix(mintResp.Code, "cvpc_") {
		t.Fatalf("pair code should have cvpc_ prefix, got %q", mintResp.Code)
	}

	// Helper: post /api/plugin/pair with a given body + optional idem key.
	doPair := func(body map[string]any, idemKey string) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, "/api/plugin/pair", bytes.NewReader(raw))
		if idemKey != "" {
			req.Header.Set("Idempotency-Key", idemKey)
		}
		rec := httptest.NewRecorder()
		h.RequestPair(rec, req)
		return rec
	}

	// Step 1: first pair attempt succeeds, code is consumed.
	body := map[string]any{
		"pair_code":           mintResp.Code,
		"install_fingerprint": "fp_1",
		"hostname":            "alice-laptop",
		"agent_ids":           []string{"main"},
	}
	rec := doPair(body, "idem-1")
	if rec.Code != http.StatusCreated {
		t.Fatalf("first pair status=%d body=%s", rec.Code, rec.Body.String())
	}
	var first struct {
		PairID string `json:"pair_id"`
	}
	_ = json.NewDecoder(rec.Body).Decode(&first)
	if first.PairID == "" {
		t.Fatal("pair_id should be populated")
	}

	// Step 2: idempotent retry with same key returns the SAME pair_id, no
	// new pending card created.
	rec = doPair(body, "idem-1")
	if rec.Code != http.StatusCreated {
		t.Fatalf("idempotent retry status=%d", rec.Code)
	}
	var second struct {
		PairID string `json:"pair_id"`
	}
	_ = json.NewDecoder(rec.Body).Decode(&second)
	if second.PairID != first.PairID {
		t.Fatalf("idempotent retry should return same pair_id; got %q vs %q", second.PairID, first.PairID)
	}
	pendings, _ := st.ListPendingPluginPairRequests(ctx, user.ID)
	if len(pendings) != 1 {
		t.Fatalf("idempotent retry must not create a duplicate pending card; got %d", len(pendings))
	}

	// Step 3: a fresh attempt (no idempotency key) with the same code must
	// fail — the code was consumed by step 1.
	rec = doPair(body, "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for reused pair_code, got %d body=%s", rec.Code, rec.Body.String())
	}

	// Step 4: an unknown code must also fail with 400 — single error path
	// so an attacker can't probe for "used" vs "unknown" vs "expired".
	rec = doPair(map[string]any{
		"pair_code":           "cvpc_" + strings.Repeat("0", 18),
		"install_fingerprint": "fp_2",
		"agent_ids":           []string{"x"},
	}, "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for unknown pair_code, got %d", rec.Code)
	}
}

// TestPluginPair_ConsumeExpiredCodeRejected confirms the ConsumePluginPairCode
// atomic check in the store — expired codes are never handed back, even if
// they exist and are unconsumed.
func TestPluginPair_ConsumeExpiredCodeRejected(t *testing.T) {
	st := newTestStore(t)
	user := mustUser(t, st, "alice@test.example")
	ctx := context.Background()

	raw, _ := auth.GeneratePluginPairCode()
	pc := &store.PluginPairCode{
		UserID:    user.ID,
		CodeHash:  auth.HashToken(raw),
		ExpiresAt: time.Now().Add(-time.Minute), // already expired
	}
	if err := st.CreatePluginPairCode(ctx, pc); err != nil {
		t.Fatalf("CreatePluginPairCode: %v", err)
	}
	if _, err := st.ConsumePluginPairCode(ctx, pc.CodeHash); err == nil {
		t.Fatal("consuming expired code should fail with ErrNotFound")
	}
}

// TestBufferForBridge_ScopesByUserAndBridge confirms the dump endpoint
// only surfaces the caller's own bridge buffer — cross-user scan is
// impossible (the prefix includes user_id) and cross-bridge-in-same-user
// traffic is filtered by BridgeID so the dashboard view doesn't mix
// installs.
func TestBufferForBridge_ScopesByUserAndBridge(t *testing.T) {
	st := newTestStore(t)
	alice := mustUser(t, st, "alice@test.example")
	bob := mustUser(t, st, "bob@test.example")

	aliceRaw, _ := auth.GenerateBridgeToken()
	aliceBridge := &store.BridgeToken{UserID: alice.ID, TokenHash: auth.HashToken(aliceRaw)}
	_ = st.CreateBridgeToken(context.Background(), aliceBridge)
	aliceBridge, _ = st.GetBridgeTokenByHash(context.Background(), aliceBridge.TokenHash)

	bobRaw, _ := auth.GenerateBridgeToken()
	bobBridge := &store.BridgeToken{UserID: bob.ID, TokenHash: auth.HashToken(bobRaw)}
	_ = st.CreateBridgeToken(context.Background(), bobBridge)
	bobBridge, _ = st.GetBridgeTokenByHash(context.Background(), bobBridge.TokenHash)

	buf := groupchat.NewMessageBuffer(20, 15*time.Minute)
	now := time.Now()
	// Alice has one conversation with one bridge-attributed message and one
	// unattributed (older, from another path). Bob has a message in her
	// prefix-match range (should NOT leak).
	buf.Append(groupchat.UserScopedKey(alice.ID, "webchat"), groupchat.BufferedMessage{
		Text: "alice says hi", SenderName: "alice", Role: "user",
		Timestamp: now, BridgeID: aliceBridge.ID, EventID: "ev-1", Seq: 1,
	})
	buf.Append(groupchat.UserScopedKey(alice.ID, "webchat"), groupchat.BufferedMessage{
		Text: "sneaky from other bridge", SenderName: "x", Role: "user",
		Timestamp: now, BridgeID: "some-other-bridge", EventID: "ev-2", Seq: 2,
	})
	buf.Append(groupchat.UserScopedKey(bob.ID, "webchat"), groupchat.BufferedMessage{
		Text: "bob says hi", SenderName: "bob", Role: "user",
		Timestamp: now, BridgeID: bobBridge.ID, EventID: "ev-3", Seq: 3,
	})

	h := NewPluginPairingHandler(st, nil, slog.Default())
	h.SetBuffer(buf)

	req := httptest.NewRequest(http.MethodGet, "/api/plugin/bridges/"+aliceBridge.ID+"/buffer", nil)
	req.SetPathValue("id", aliceBridge.ID)
	req = withUserContext(req, alice)
	rec := httptest.NewRecorder()
	h.BufferForBridge(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		BridgeID      string                     `json:"bridge_id"`
		Conversations map[string][]map[string]any `json:"conversations"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.BridgeID != aliceBridge.ID {
		t.Fatalf("bridge_id mismatch: got %s", resp.BridgeID)
	}
	entries := resp.Conversations["webchat"]
	if len(entries) != 1 {
		t.Fatalf("expected exactly one entry attributable to alice's bridge; got %d (%+v)", len(entries), entries)
	}
	if entries[0]["text"] != "alice says hi" {
		t.Fatalf("wrong entry surfaced: %+v", entries[0])
	}
	if entries[0]["event_id"] != "ev-1" {
		t.Fatalf("event_id should round-trip: %+v", entries[0])
	}

	// Bob must not be able to dump Alice's bridge.
	req2 := httptest.NewRequest(http.MethodGet, "/api/plugin/bridges/"+aliceBridge.ID+"/buffer", nil)
	req2.SetPathValue("id", aliceBridge.ID)
	req2 = withUserContext(req2, bob)
	rec2 := httptest.NewRecorder()
	h.BufferForBridge(rec2, req2)
	if rec2.Code != http.StatusForbidden {
		t.Fatalf("cross-user dump should 403, got %d body=%s", rec2.Code, rec2.Body.String())
	}
}

// TestPluginAgentAdd_UsesBridgeTokenIdentity verifies the post-pair agent-add
// path: a bridge-authenticated request creates a pending pair_request with
// bridge_token_id pre-populated, so the dashboard renders "agent add" rather
// than a fresh plugin pairing.
func TestPluginAgentAdd_UsesBridgeTokenIdentity(t *testing.T) {
	st := newTestStore(t)
	user := mustUser(t, st, "alice@test.example")
	rawBridge, _ := auth.GenerateBridgeToken()
	bt := &store.BridgeToken{
		UserID:             user.ID,
		TokenHash:          auth.HashToken(rawBridge),
		InstallFingerprint: "fp_alice_laptop",
		Hostname:           "alice-laptop",
	}
	_ = st.CreateBridgeToken(context.Background(), bt)

	h := NewPluginPairingHandler(st, nil, slog.Default())

	body, _ := json.Marshal(map[string]any{"agent_id": "new-agent"})
	req := httptest.NewRequest(http.MethodPost, "/api/plugin/agents", bytes.NewReader(body))
	req = req.WithContext(store.WithBridge(req.Context(), bt))
	rec := httptest.NewRecorder()

	h.RequestAgentAdd(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	reqs, _ := st.ListPendingPluginPairRequests(context.Background(), user.ID)
	if len(reqs) != 1 {
		t.Fatalf("expected 1 pending pair request, got %d", len(reqs))
	}
	got := reqs[0]
	if got.BridgeTokenID != bt.ID {
		t.Fatalf("agent-add should inherit bridge_token_id=%s, got %q", bt.ID, got.BridgeTokenID)
	}
	if len(got.AgentIDs) != 1 || got.AgentIDs[0] != "new-agent" {
		t.Fatalf("agent_ids mismatch: %+v", got.AgentIDs)
	}
	if got.InstallFingerprint != bt.InstallFingerprint || got.Hostname != bt.Hostname {
		t.Fatalf("install identity not inherited from bridge: %+v", got)
	}
}
