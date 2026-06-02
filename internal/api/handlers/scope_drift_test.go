package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// requestWithAgent injects an authenticated agent into the request
// context the same way middleware.RequireAgentLLMNonce does in
// production. Named distinctly from local_service_test.go's withAgent
// (which takes a different signature) so both can coexist in the
// handlers test package.
func requestWithAgent(req *http.Request, agentID, userID string) *http.Request {
	ctx := store.WithAgent(req.Context(), &store.Agent{ID: agentID, UserID: userID})
	return req.WithContext(ctx)
}

// readJSON decodes the response body or fails the test.
func readJSON(t *testing.T, body io.Reader) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.NewDecoder(body).Decode(&out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return out
}

func TestScopeDriftOneOff_ReturnsNotImplementedWithoutClaiming(t *testing.T) {
	reg := llmproxy.NewMemoryScopeDriftRegistry(60_000_000_000) // 60s
	ctx := context.Background()

	// Register a drift so we can verify it stays unclaimed after the 501.
	drift, err := reg.Register(ctx, llmproxy.ScopeDrift{
		AgentID: "agent-1",
		UserID:  "user-1",
		Service: "github",
		Action:  "create_issue",
		Source:  llmproxy.ScopeDriftSourceIntentVerification,
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	h := NewScopeDriftHandler(reg, nil, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/control/scope-drift/"+drift.ID+"/one-off",
		bytes.NewBufferString(`{"agent_note":"throwaway probe"}`))
	req.SetPathValue("id", drift.ID)
	req = requestWithAgent(req, "agent-1", "user-1")
	w := httptest.NewRecorder()

	h.OneOff(w, req)

	if w.Code != http.StatusNotImplemented {
		t.Fatalf("expected 501, got %d (body=%s)", w.Code, w.Body.String())
	}
	out := readJSON(t, w.Body)
	if out["error"] != "one_off_not_implemented" {
		t.Errorf("expected error one_off_not_implemented, got %v", out["error"])
	}

	// Critical assertion: the 501 must NOT consume the one-shot cap.
	// The drift should still be claimable for option (d) afterwards.
	post, err := reg.Get(ctx, drift.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if post.ChosenOption != "" {
		t.Fatalf("OneOff 501 burned the one-shot cap; ChosenOption=%q", post.ChosenOption)
	}
}

func TestScopeDriftJustify_NilVerifierDoesNotBurnOneShotCap(t *testing.T) {
	reg := llmproxy.NewMemoryScopeDriftRegistry(60_000_000_000)
	ctx := context.Background()
	drift, _ := reg.Register(ctx, llmproxy.ScopeDrift{
		AgentID: "agent-1",
		UserID:  "user-1",
		Service: "github",
		Action:  "create_issue",
		Source:  llmproxy.ScopeDriftSourceIntentVerification,
	})

	// Verifier deliberately nil — emulates a misconfigured daemon.
	h := NewScopeDriftHandler(reg, nil, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/control/scope-drift/"+drift.ID+"/justify",
		bytes.NewBufferString(`{"justification":"the call fits the active task purpose because X"}`))
	req.SetPathValue("id", drift.ID)
	req = requestWithAgent(req, "agent-1", "user-1")
	w := httptest.NewRecorder()

	h.Justify(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d (body=%s)", w.Code, w.Body.String())
	}
	out := readJSON(t, w.Body)
	if out["error"] != "verifier_unavailable" {
		t.Errorf("expected error verifier_unavailable, got %v", out["error"])
	}

	// Critical assertion: the 503 must NOT consume the one-shot cap.
	// Cubic flagged this exact regression — claiming the drift before
	// the verifier check would permanently lock the agent out of
	// (a)/(b) for a path that never ran.
	post, err := reg.Get(ctx, drift.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if post.ChosenOption != "" {
		t.Fatalf("Justify 503 burned the one-shot cap; ChosenOption=%q", post.ChosenOption)
	}
}
