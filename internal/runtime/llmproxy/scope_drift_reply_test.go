package llmproxy

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// driftReplyFixture sets up the registry + cache + drift + pending hold
// state that mirrors what applyScopeDriftDecisions produces after the
// agent emits a one-off markup. Tests exercise the user's reply rewrite
// without going through applyScopeDriftDecisions first.
type driftReplyFixture struct {
	registry ScopeDriftRegistry
	pending  PendingApprovalCache
	drift    ScopeDrift
	hold     PendingLiteApproval
}

func newDriftReplyFixture(t *testing.T) driftReplyFixture {
	t.Helper()
	reg := NewMemoryScopeDriftRegistry(0)
	pending := NewMemoryPendingApprovalCache(0)
	ctx := context.Background()
	drift, err := reg.Register(ctx, ScopeDrift{
		AgentID: "agent-1",
		UserID:  "user-1",
		Service: "github",
		Action:  "create_issue",
		Host:    "api.github.com",
		Method:  "POST",
		Path:    "/repos/x/y/issues",
		Source:  ScopeDriftSourceIntentVerification,
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, err := reg.ClaimOption(ctx, drift.ID, ScopeDriftOptionOneOff, "diag probe", ""); err != nil {
		t.Fatalf("ClaimOption: %v", err)
	}
	// Refresh the drift snapshot so callers see ChosenOption set.
	drift, _ = reg.Get(ctx, drift.ID)
	heldResult, err := pending.Hold(ctx, PendingLiteApproval{
		UserID:              "user-1",
		AgentID:             "agent-1",
		Provider:            conversation.ProviderAnthropic,
		Stage:               StageAwaitingScopeDriftOneOff,
		Reason:              "scope-drift one-off probe",
		ScopeDriftID:        drift.ID,
		ScopeDriftAgentNote: "diag probe",
	})
	if err != nil {
		t.Fatalf("Hold: %v", err)
	}
	return driftReplyFixture{
		registry: reg,
		pending:  pending,
		drift:    drift,
		hold:     heldResult.Pending,
	}
}

func TestRewriteScopeDriftOneOffApprovalReply_Approve_InsertsPreClear(t *testing.T) {
	fix := newDriftReplyFixture(t)
	ctx := context.Background()

	req := httptest.NewRequest("POST", "/api/v1/messages", nil)
	body := []byte(`{"messages":[{"role":"user","content":"approve ` + fix.hold.ID + `"}]}`)

	out, err := RewriteScopeDriftOneOffApprovalReply(ctx, ScopeDriftReplyRewriteRequest{
		HTTPRequest:     req,
		Provider:        conversation.ProviderAnthropic,
		Body:            body,
		Agent:           &store.Agent{ID: "agent-1", UserID: "user-1"},
		PendingApproval: fix.pending,
		ScopeDrifts:     fix.registry,
	})
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if !out.Rewritten {
		t.Fatalf("expected rewritten=true, got body unchanged")
	}
	if out.Decision != "allow" {
		t.Errorf("expected decision=allow, got %q", out.Decision)
	}
	if out.DriftID != fix.drift.ID {
		t.Errorf("expected DriftID=%q, got %q", fix.drift.ID, out.DriftID)
	}
	// Pre-clear must be consumable exactly once.
	if _, ok := fix.registry.LookupPreClear(ctx, "agent-1", fix.drift.Fingerprint()); !ok {
		t.Errorf("expected pre-clear after approve")
	}
	if _, ok := fix.registry.LookupPreClear(ctx, "agent-1", fix.drift.Fingerprint()); ok {
		t.Errorf("pre-clear should be one-shot")
	}
	// The hold should have been consumed.
	if held, _ := fix.pending.Peek(ctx, ResolveRequest{
		UserID: "user-1", AgentID: "agent-1", Provider: conversation.ProviderAnthropic, ApprovalID: fix.hold.ID,
	}); held != nil {
		t.Errorf("hold was not consumed after approval")
	}
	// Drift outcome flipped to succeeded.
	updated, _ := fix.registry.Get(ctx, fix.drift.ID)
	if updated.Outcome != ScopeDriftOutcomeSucceeded {
		t.Errorf("expected outcome=succeeded, got %q", updated.Outcome)
	}
	// User message replaced with a Clawvisor scope-drift status line
	// so the model sees coherent context, not a bare "approve".
	if !strings.Contains(string(out.Body), "[Clawvisor scope-drift]") {
		t.Errorf("rewritten body missing status marker:\n%s", out.Body)
	}
}

func TestRewriteScopeDriftOneOffApprovalReply_Deny_ClosesDriftNoPreClear(t *testing.T) {
	fix := newDriftReplyFixture(t)
	ctx := context.Background()

	req := httptest.NewRequest("POST", "/api/v1/messages", nil)
	body := []byte(`{"messages":[{"role":"user","content":"deny ` + fix.hold.ID + `"}]}`)

	out, err := RewriteScopeDriftOneOffApprovalReply(ctx, ScopeDriftReplyRewriteRequest{
		HTTPRequest:     req,
		Provider:        conversation.ProviderAnthropic,
		Body:            body,
		Agent:           &store.Agent{ID: "agent-1", UserID: "user-1"},
		PendingApproval: fix.pending,
		ScopeDrifts:     fix.registry,
	})
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if !out.Rewritten {
		t.Fatalf("expected rewritten=true")
	}
	if out.Decision != "deny" {
		t.Errorf("expected decision=deny, got %q", out.Decision)
	}
	if _, ok := fix.registry.LookupPreClear(ctx, "agent-1", fix.drift.Fingerprint()); ok {
		t.Errorf("pre-clear inserted on deny")
	}
	updated, _ := fix.registry.Get(ctx, fix.drift.ID)
	if updated.Outcome != ScopeDriftOutcomeDenied {
		t.Errorf("expected outcome=denied, got %q", updated.Outcome)
	}
}

func TestRewriteScopeDriftOneOffApprovalReply_NoMatchingHoldIsNoOp(t *testing.T) {
	reg := NewMemoryScopeDriftRegistry(0)
	pending := NewMemoryPendingApprovalCache(0)
	req := httptest.NewRequest("POST", "/api/v1/messages", nil)
	body := []byte(`{"messages":[{"role":"user","content":"approve"}]}`)

	out, err := RewriteScopeDriftOneOffApprovalReply(context.Background(), ScopeDriftReplyRewriteRequest{
		HTTPRequest:     req,
		Provider:        conversation.ProviderAnthropic,
		Body:            body,
		Agent:           &store.Agent{ID: "agent-1", UserID: "user-1"},
		PendingApproval: pending,
		ScopeDrifts:     reg,
	})
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if out.Rewritten {
		t.Errorf("expected no rewrite when no matching hold exists")
	}
}

func TestRewriteScopeDriftOneOffApprovalReply_NonScopeDriftHoldDefersToInlineTask(t *testing.T) {
	// A non-scope-drift hold (e.g. an awaiting_task_approval one) must
	// pass through this rewriter unchanged so the inline-task rewriter
	// downstream can handle it.
	pending := NewMemoryPendingApprovalCache(0)
	reg := NewMemoryScopeDriftRegistry(0)
	ctx := context.Background()
	heldResult, _ := pending.Hold(ctx, PendingLiteApproval{
		UserID:   "user-1",
		AgentID:  "agent-1",
		Provider: conversation.ProviderAnthropic,
		Stage:    StageAwaitingTaskApproval,
	})

	req := httptest.NewRequest("POST", "/api/v1/messages", nil)
	body := []byte(`{"messages":[{"role":"user","content":"approve ` + heldResult.Pending.ID + `"}]}`)

	out, err := RewriteScopeDriftOneOffApprovalReply(ctx, ScopeDriftReplyRewriteRequest{
		HTTPRequest:     req,
		Provider:        conversation.ProviderAnthropic,
		Body:            body,
		Agent:           &store.Agent{ID: "agent-1", UserID: "user-1"},
		PendingApproval: pending,
		ScopeDrifts:     reg,
	})
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if out.Rewritten {
		t.Errorf("scope-drift rewriter should not consume an inline-task hold")
	}
	// Hold must still be there for the inline-task rewriter to find.
	if held, _ := pending.Peek(ctx, ResolveRequest{
		UserID: "user-1", AgentID: "agent-1", Provider: conversation.ProviderAnthropic, ApprovalID: heldResult.Pending.ID,
	}); held == nil {
		t.Errorf("inline-task hold was consumed by scope-drift rewriter")
	}
}
