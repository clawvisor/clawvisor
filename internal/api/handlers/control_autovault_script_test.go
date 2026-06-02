package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/clawvisor/clawvisor/internal/api/middleware"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy"
	"github.com/clawvisor/clawvisor/pkg/store"
)

func newSeededControlHandler(t *testing.T) (*LLMControlHandler, store.Store, *store.Agent, llmproxy.CallerNonceCache, llmproxy.ScriptSessionCache, string) {
	t.Helper()
	_, st, _, agent, nonces, placeholder := newSeededResolver(t)
	scripts := llmproxy.NewMemoryScriptSessionCache()
	h := NewLLMControlHandler("http://clawvisor.example")
	h.Store = st
	h.ScriptSessions = scripts
	return h, st, agent, nonces, scripts, placeholder
}

func wrapWithAuth(t *testing.T, st store.Store, nonces llmproxy.CallerNonceCache, scripts llmproxy.ScriptSessionCache, handler http.Handler) http.Handler {
	t.Helper()
	mw := middleware.RequireAgentLLMNonce(st, nonces, scripts, nil)
	return mw(handler)
}

func mintRequest(t *testing.T, body any, nonces llmproxy.CallerNonceCache, agentID string) *http.Request {
	t.Helper()
	buf, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/control/autovault/script-session", bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Clawvisor-Target-Host", "clawvisor.local")
	nonce, err := nonces.Mint(context.Background(), agentID, llmproxy.NonceTarget{
		Host:   "clawvisor.local",
		Method: http.MethodPost,
		Path:   "/api/control/autovault/script-session",
	})
	if err != nil {
		t.Fatalf("mint nonce: %v", err)
	}
	req.Header.Set("X-Clawvisor-Caller", "Bearer "+nonce)
	return req
}

func TestMintScriptSession_HappyPath(t *testing.T) {
	h, st, agent, nonces, scripts, placeholder := newSeededControlHandler(t)

	req := mintRequest(t, map[string]any{
		"placeholder":   placeholder,
		"target_host":   "api.github.com",
		"methods":       []string{"GET"},
		"path_prefixes": []string{"/repos/x/y"},
		"max_uses":      5,
		"ttl_seconds":   60,
		"why":           "Fetch issue metadata for triage.",
	}, nonces, agent.ID)
	rec := httptest.NewRecorder()
	wrapWithAuth(t, st, nonces, scripts, http.HandlerFunc(h.MintScriptSession)).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	tok, _ := resp["caller_token"].(string)
	if !strings.HasPrefix(tok, llmproxy.ScriptSessionPrefix) {
		t.Fatalf("caller_token missing prefix: %q", tok)
	}
	if resp["base_url"] != "http://clawvisor.example/api/proxy" {
		t.Errorf("unexpected base_url: %v", resp["base_url"])
	}
	if resp["target_host"] != "api.github.com" {
		t.Errorf("unexpected target_host: %v", resp["target_host"])
	}
}

func TestMintScriptSession_RejectsBadHost(t *testing.T) {
	h, st, agent, nonces, scripts, placeholder := newSeededControlHandler(t)

	req := mintRequest(t, map[string]any{
		"placeholder":   placeholder,
		"target_host":   "evil.example",
		"methods":       []string{"GET"},
		"path_prefixes": []string{"/x"},
		"max_uses":      5,
		"ttl_seconds":   60,
		"why":           "bad",
	}, nonces, agent.ID)
	rec := httptest.NewRecorder()
	wrapWithAuth(t, st, nonces, scripts, http.HandlerFunc(h.MintScriptSession)).ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 target_host_not_bound, got %d (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "target_host_not_bound") {
		t.Fatalf("expected target_host_not_bound code, got %s", rec.Body.String())
	}
}

func TestMintScriptSession_RejectsWriteMethod(t *testing.T) {
	h, st, agent, nonces, scripts, placeholder := newSeededControlHandler(t)

	req := mintRequest(t, map[string]any{
		"placeholder":   placeholder,
		"target_host":   "api.github.com",
		"methods":       []string{"POST"},
		"path_prefixes": []string{"/repos/x/y"},
		"max_uses":      5,
		"ttl_seconds":   60,
		"why":           "write",
	}, nonces, agent.ID)
	rec := httptest.NewRecorder()
	wrapWithAuth(t, st, nonces, scripts, http.HandlerFunc(h.MintScriptSession)).ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 invalid_methods, got %d (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "invalid_methods") {
		t.Fatalf("expected invalid_methods code, got %s", rec.Body.String())
	}
}

func TestMintScriptSession_RejectsOverlimitTTL(t *testing.T) {
	h, st, agent, nonces, scripts, placeholder := newSeededControlHandler(t)

	req := mintRequest(t, map[string]any{
		"placeholder":   placeholder,
		"target_host":   "api.github.com",
		"methods":       []string{"GET"},
		"path_prefixes": []string{"/repos/x/y"},
		"max_uses":      5,
		"ttl_seconds":   600,
		"why":           "long",
	}, nonces, agent.ID)
	rec := httptest.NewRecorder()
	wrapWithAuth(t, st, nonces, scripts, http.HandlerFunc(h.MintScriptSession)).ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 invalid_ttl, got %d (%s)", rec.Code, rec.Body.String())
	}
}

func TestMintScriptSession_RejectsRootPathPrefix(t *testing.T) {
	h, st, agent, nonces, scripts, placeholder := newSeededControlHandler(t)

	req := mintRequest(t, map[string]any{
		"placeholder":   placeholder,
		"target_host":   "api.github.com",
		"methods":       []string{"GET"},
		"path_prefixes": []string{"/"},
		"max_uses":      5,
		"ttl_seconds":   60,
		"why":           "bad scope",
	}, nonces, agent.ID)
	rec := httptest.NewRecorder()
	wrapWithAuth(t, st, nonces, scripts, http.HandlerFunc(h.MintScriptSession)).ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 invalid_path_prefixes, got %d (%s)", rec.Code, rec.Body.String())
	}
}

func TestMintScriptSession_RejectsUnknownPlaceholder(t *testing.T) {
	h, st, agent, nonces, scripts, _ := newSeededControlHandler(t)

	req := mintRequest(t, map[string]any{
		"placeholder":   "autovault_does_not_exist",
		"target_host":   "api.github.com",
		"methods":       []string{"GET"},
		"path_prefixes": []string{"/repos/x/y"},
		"max_uses":      5,
		"ttl_seconds":   60,
		"why":           "unknown",
	}, nonces, agent.ID)
	rec := httptest.NewRecorder()
	wrapWithAuth(t, st, nonces, scripts, http.HandlerFunc(h.MintScriptSession)).ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 unknown_placeholder, got %d (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "unknown_placeholder") {
		t.Fatalf("expected unknown_placeholder code, got %s", rec.Body.String())
	}
}

// TestMintScriptSession_PersistsSessionForResolver checks that a minted
// session is immediately usable by the resolver branch — closing the
// loop between Mint and Authorize across the cache.
func TestMintScriptSession_PersistsSessionForResolver(t *testing.T) {
	h, st, agent, nonces, scripts, placeholder := newSeededControlHandler(t)

	req := mintRequest(t, map[string]any{
		"placeholder":   placeholder,
		"target_host":   "api.github.com",
		"methods":       []string{"GET"},
		"path_prefixes": []string{"/repos/x/y"},
		"max_uses":      5,
		"ttl_seconds":   60,
		"why":           "Fetch repo metadata.",
	}, nonces, agent.ID)
	rec := httptest.NewRecorder()
	wrapWithAuth(t, st, nonces, scripts, http.HandlerFunc(h.MintScriptSession)).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("mint failed: %d (%s)", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	tok, _ := resp["caller_token"].(string)
	if tok == "" {
		t.Fatalf("no caller_token in response: %s", rec.Body.String())
	}

	got, err := scripts.Authorize(context.Background(), tok, llmproxy.ScriptSessionRequest{
		Host: "api.github.com", Method: "GET",
		Path: "/repos/x/y/issues", Placeholder: placeholder,
	})
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	if got.AgentID != agent.ID {
		t.Errorf("expected AgentID=%s, got %s", agent.ID, got.AgentID)
	}
	if got.ExpiresAt.Before(time.Now()) {
		t.Errorf("session expired immediately: %v", got.ExpiresAt)
	}
}
