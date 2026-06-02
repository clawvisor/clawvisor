package llmproxy

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestScopeDriftRegistry_RegisterAndGet(t *testing.T) {
	r := NewMemoryScopeDriftRegistry(60 * time.Second)
	ctx := context.Background()

	stored, err := r.Register(ctx, ScopeDrift{
		UserID:  "user-1",
		AgentID: "agent-1",
		Service: "github",
		Action:  "create_issue",
		Source:  ScopeDriftSourceIntentVerification,
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if stored.ID == "" {
		t.Fatal("Register did not mint an ID")
	}
	if stored.CreatedAt.IsZero() || stored.ExpiresAt.IsZero() {
		t.Fatal("Register did not stamp timestamps")
	}
	if !strings.HasPrefix(stored.ID, "drift-") {
		t.Fatalf("Register minted unexpected ID shape: %q", stored.ID)
	}

	round, err := r.Get(ctx, stored.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if round.Service != "github" || round.Action != "create_issue" {
		t.Fatalf("Get returned wrong record: %+v", round)
	}
}

func TestScopeDriftRegistry_OneShotCap(t *testing.T) {
	r := NewMemoryScopeDriftRegistry(60 * time.Second)
	ctx := context.Background()

	stored, _ := r.Register(ctx, ScopeDrift{
		AgentID: "agent-1",
		Service: "svc",
		Action:  "act",
		Source:  ScopeDriftSourceIntentVerification,
	})
	// First claim succeeds.
	if _, err := r.ClaimOption(ctx, stored.ID, ScopeDriftOptionJustify, "", "because it fits"); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	// Second claim refused even with a different option.
	_, err := r.ClaimOption(ctx, stored.ID, ScopeDriftOptionOneOff, "throwaway", "")
	if err != ErrDriftAlreadyResolved {
		t.Fatalf("second claim should be refused, got: %v", err)
	}
}

func TestScopeDriftRegistry_PreClearLifecycle(t *testing.T) {
	r := NewMemoryScopeDriftRegistry(60 * time.Second)
	ctx := context.Background()

	stored, _ := r.Register(ctx, ScopeDrift{
		AgentID: "agent-1",
		Service: "svc",
		Action:  "act",
		Host:    "api.example.com",
		Method:  "POST",
		Path:    "/widgets",
		Source:  ScopeDriftSourceIntentVerification,
	})

	// No pre-clear before SetOutcome(succeeded).
	if _, ok := r.LookupPreClear(ctx, "agent-1", stored.Fingerprint()); ok {
		t.Fatal("unexpected pre-clear before SetOutcome")
	}

	// Claim then mark succeeded; pre-clear available exactly once.
	if _, err := r.ClaimOption(ctx, stored.ID, ScopeDriftOptionJustify, "", "fits"); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := r.SetOutcome(ctx, stored.ID, ScopeDriftOutcomeSucceeded); err != nil {
		t.Fatalf("SetOutcome: %v", err)
	}
	if _, ok := r.LookupPreClear(ctx, "agent-1", stored.Fingerprint()); !ok {
		t.Fatal("pre-clear missing after SetOutcome(succeeded)")
	}
	if _, ok := r.LookupPreClear(ctx, "agent-1", stored.Fingerprint()); ok {
		t.Fatal("pre-clear should be one-shot")
	}
}

func TestScopeDriftRegistry_TTLExpiry(t *testing.T) {
	r := NewMemoryScopeDriftRegistry(50 * time.Millisecond)
	ctx := context.Background()

	stored, _ := r.Register(ctx, ScopeDrift{
		AgentID: "agent-1",
		Service: "svc",
		Action:  "act",
		Source:  ScopeDriftSourceIntentVerification,
	})
	time.Sleep(80 * time.Millisecond)
	if _, err := r.Get(ctx, stored.ID); err != ErrDriftNotFound {
		t.Fatalf("expected ErrDriftNotFound after TTL, got %v", err)
	}
}

func TestRenderScopeDriftMenu_VisibleOptions(t *testing.T) {
	out := renderScopeDriftMenu(MenuFields{
		DriftID:    "drift-abc",
		Service:    "github",
		Action:     "create_issue",
		TaskID:     "task-1",
		ReasonText: "params violate scope",
		Source:     ScopeDriftSourceIntentVerification,
	}, "https://clawvisor.local")

	mustContain := []string{
		"Drift ID: drift-abc",
		"github.create_issue",
		"params violate scope",
		"(a) Expand the active task",
		"/control/tasks/task-1/expand?surface=inline",
		"(b) Create a new task",
		"/control/tasks?surface=inline",
		"(d) False positive",
		"/control/scope-drift/drift-abc/justify",
		"You do not get to retry against another option",
	}
	for _, s := range mustContain {
		if !strings.Contains(out, s) {
			t.Errorf("menu missing substring %q\n--- rendered ---\n%s", s, out)
		}
	}
	// Option (c) is documented in the control skill schema but
	// deliberately omitted from the rendered menu on this build —
	// the user-approval channel that flips its outcome is not yet
	// wired, so advertising it would invite the agent to burn the
	// one-shot cap on a path that cannot complete.
	mustNotContain := []string{
		"(c) One-off",
		"/control/scope-drift/drift-abc/one-off",
	}
	for _, s := range mustNotContain {
		if strings.Contains(out, s) {
			t.Errorf("menu unexpectedly contains substring %q\n--- rendered ---\n%s", s, out)
		}
	}
}

func TestRenderScopeDriftMenu_NoActiveTaskHidesExpandURL(t *testing.T) {
	out := renderScopeDriftMenu(MenuFields{
		DriftID:    "drift-xyz",
		Service:    "github",
		Action:     "create_issue",
		ReasonText: "no_active_task",
		Source:     ScopeDriftSourceTaskScope,
	}, "https://clawvisor.local")

	if strings.Contains(out, "/control/tasks//expand") {
		t.Errorf("menu rendered an expand URL with no task_id:\n%s", out)
	}
	if !strings.Contains(out, "No active task was matched at block time") {
		t.Errorf("menu missing guidance when no task matched:\n%s", out)
	}
}
