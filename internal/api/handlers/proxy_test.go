package handlers

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/clawvisor/clawvisor/internal/auth"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// bootstrapProxy mints a bridge + proxy instance and returns all of
// (user, bridge-token raw, proxy-token raw, proxy instance). Used as
// common setup for the proxy-handler tests below.
func bootstrapProxy(t *testing.T, st store.Store) (*store.User, *store.BridgeToken, string, string) {
	t.Helper()
	ctx := context.Background()

	u := mustUser(t, st, "alice@test")

	bridgeRaw, _ := auth.GenerateBridgeToken()
	bt := &store.BridgeToken{
		UserID:    u.ID,
		TokenHash: auth.HashToken(bridgeRaw),
		Hostname:  "alice-host",
	}
	if err := st.CreateBridgeToken(ctx, bt); err != nil {
		t.Fatalf("CreateBridgeToken: %v", err)
	}

	proxyRaw, _ := auth.GenerateProxyToken()
	pi := &store.ProxyInstance{
		BridgeID:  bt.ID,
		TokenHash: auth.HashToken(proxyRaw),
	}
	if err := st.CreateProxyInstance(ctx, pi); err != nil {
		t.Fatalf("CreateProxyInstance: %v", err)
	}

	// Enable proxy on bridge so /api/proxy/config doesn't reject.
	if err := st.SetBridgeProxyEnabled(ctx, bt.ID, u.ID, true); err != nil {
		t.Fatalf("SetBridgeProxyEnabled: %v", err)
	}

	// Refresh BridgeToken to pick up the proxy_enabled flip.
	freshBT, _ := st.GetBridgeTokenByID(ctx, bt.ID)
	return u, freshBT, bridgeRaw, proxyRaw
}

func TestProxyHandler_Config_ReturnsAgents(t *testing.T) {
	st := newTestStore(t)
	u, bt, _, proxyRaw := bootstrapProxy(t, st)

	// Seed an agent so the config response has something to return.
	agentRaw, _ := auth.GenerateAgentToken()
	if _, err := st.CreateAgent(context.Background(), u.ID, "main", auth.HashToken(agentRaw)); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}

	h := NewProxyHandler(st, slog.Default())
	p, err := st.GetProxyInstanceByHash(context.Background(), auth.HashToken(proxyRaw))
	if err != nil {
		t.Fatalf("get proxy: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/proxy/config", nil)
	req = req.WithContext(store.WithProxy(req.Context(), p))
	rw := httptest.NewRecorder()
	h.Config(rw, req)

	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rw.Code, rw.Body.String())
	}
	var resp proxyConfigResponse
	if err := json.NewDecoder(rw.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.BridgeID != bt.ID {
		t.Errorf("bridge_id mismatch: got %q", resp.BridgeID)
	}
	if len(resp.Agents) != 1 || resp.Agents[0].AgentLabel != "main" {
		t.Errorf("agents: %+v", resp.Agents)
	}
	if resp.ContractVersion != "v1-draft" {
		t.Errorf("contract_version: got %q", resp.ContractVersion)
	}
}

func TestProxyHandler_Turns_IngestsEvents(t *testing.T) {
	st := newTestStore(t)
	_, bt, _, proxyRaw := bootstrapProxy(t, st)

	h := NewProxyHandler(st, slog.Default())
	p, _ := st.GetProxyInstanceByHash(context.Background(), auth.HashToken(proxyRaw))

	now := time.Now().UTC()
	events := []map[string]any{
		{
			"event_id":        "evt_test_1",
			"ts":              now.Format(time.RFC3339Nano),
			"source":          "proxy",
			"source_version":  "v1",
			"stream":          "channel",
			"agent_token_id":  "cvis_dummy",
			"bridge_id":       bt.ID,
			"conversation_id": "telegram:12345",
			"provider":        "telegram",
			"direction":       "inbound",
			"role":            "user",
			"turn":            map[string]any{"text": "hello world"},
		},
	}
	body, _ := json.Marshal(map[string]any{"events": events})
	req := httptest.NewRequest(http.MethodPost, "/api/proxy/turns", bytes.NewReader(body))
	req = req.WithContext(store.WithProxy(req.Context(), p))
	rw := httptest.NewRecorder()
	h.Turns(rw, req)

	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rw.Code, rw.Body.String())
	}
	var resp turnsIngestResponse
	if err := json.NewDecoder(rw.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Accepted != 1 || len(resp.Rejected) != 0 {
		t.Errorf("expected 1 accepted, 0 rejected; got %+v", resp)
	}

	stored, err := st.GetTranscriptEventByID(context.Background(), "evt_test_1")
	if err != nil {
		t.Fatalf("event not stored: %v", err)
	}
	if stored.Text != "hello world" || stored.Role != "user" || stored.Source != "proxy" {
		t.Errorf("stored event mismatch: %+v", stored)
	}
	if stored.SigStatus != "unsigned" {
		t.Errorf("expected sig_status=unsigned, got %q", stored.SigStatus)
	}
}

func TestProxyHandler_Turns_DuplicateRejected(t *testing.T) {
	st := newTestStore(t)
	_, bt, _, proxyRaw := bootstrapProxy(t, st)

	h := NewProxyHandler(st, slog.Default())
	p, _ := st.GetProxyInstanceByHash(context.Background(), auth.HashToken(proxyRaw))

	ev := map[string]any{
		"event_id":       "evt_dup",
		"ts":             time.Now().UTC().Format(time.RFC3339Nano),
		"source":         "proxy",
		"source_version": "v1",
		"stream":         "channel",
		"bridge_id":      bt.ID,
		"provider":       "telegram",
		"direction":      "inbound",
		"role":           "user",
		"turn":           map[string]any{"text": "first"},
	}
	post := func() *turnsIngestResponse {
		body, _ := json.Marshal(map[string]any{"events": []map[string]any{ev}})
		req := httptest.NewRequest(http.MethodPost, "/api/proxy/turns", bytes.NewReader(body))
		req = req.WithContext(store.WithProxy(req.Context(), p))
		rw := httptest.NewRecorder()
		h.Turns(rw, req)
		var r turnsIngestResponse
		json.NewDecoder(rw.Body).Decode(&r)
		return &r
	}

	r1 := post()
	if r1.Accepted != 1 {
		t.Fatalf("first: expected 1 accepted, got %+v", r1)
	}
	r2 := post()
	if r2.Accepted != 0 || len(r2.Rejected) != 1 || r2.Rejected[0].Code != "DUPLICATE_EVENT" {
		t.Errorf("second should dedupe: got %+v", r2)
	}
}

func TestProxyHandler_SigningKeyRotate_Idempotent(t *testing.T) {
	st := newTestStore(t)
	_, _, _, proxyRaw := bootstrapProxy(t, st)

	h := NewProxyHandler(st, slog.Default())
	p, _ := st.GetProxyInstanceByHash(context.Background(), auth.HashToken(proxyRaw))

	// Generate a valid Ed25519 public key.
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKIXPublicKey(pub)
	publicKeyB64 := base64.StdEncoding.EncodeToString(der)

	registerBody, _ := json.Marshal(map[string]string{
		"key_id":     "proxy-2026-04-19",
		"alg":        "ed25519",
		"public_key": publicKeyB64,
	})

	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodPost,
			"/api/proxy/signing-keys/rotate", bytes.NewReader(registerBody))
		req = req.WithContext(store.WithProxy(req.Context(), p))
		rw := httptest.NewRecorder()
		h.SigningKeyRotate(rw, req)
		if rw.Code != http.StatusOK {
			t.Fatalf("round %d: status = %d, body = %s", i, rw.Code, rw.Body.String())
		}
	}

	// Only one row should exist.
	keys, err := st.ListProxySigningKeys(context.Background(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 {
		t.Errorf("expected 1 signing key, got %d", len(keys))
	}
}
