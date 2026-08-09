package handlers

import (
	"context"
	"errors"
	"testing"

	"github.com/clawvisor/clawvisor/pkg/store"
)

// The hook must apply on EVERY creation path. Gating at the HTTP router only
// covered one of three: MCP tool dispatch and inline LLM-proxy creation never
// touch a task route, so an out-of-quota account kept minting tasks through
// them. guardTaskCreate is the single chokepoint all three now share.
func TestGuardTaskCreate(t *testing.T) {
	h := &TasksHandler{}
	agent := &store.Agent{ID: "a1", UserID: "u1"}

	// No hook installed (open-source build) — never refuses.
	if err := h.guardTaskCreate(context.Background(), agent); err != nil {
		t.Fatalf("no hook should allow, got %v", err)
	}

	blocked := errors.New("out of requests")
	var sawUser string
	h.SetBeforeTaskCreate(func(_ context.Context, a *store.Agent) error {
		sawUser = a.UserID
		return blocked
	})

	if err := h.guardTaskCreate(context.Background(), agent); !errors.Is(err, blocked) {
		t.Fatalf("hook error must propagate, got %v", err)
	}
	if sawUser != "u1" {
		t.Errorf("hook saw user %q, want u1 — quota is charged to the agent's owner", sawUser)
	}

	// An unauthenticated call has no subject to check; the route's own auth
	// produces the error, so the guard must not invent one.
	if err := h.guardTaskCreate(context.Background(), nil); err != nil {
		t.Errorf("nil agent should pass through to auth, got %v", err)
	}
}
