package llmproxy

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
)

func TestScopeDriftRegistry_RegisterGetClaimSetOutcomePreClear(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	reg := NewMemoryScopeDriftRegistry(0) // default TTL (10 min)

	tu := conversation.ToolUse{ID: "tu-1", Name: "Bash", Input: []byte(`{"command":"ls"}`)}
	tmpl := ScopeDrift{
		UserID:         "user-1",
		AgentID:        "agent-1",
		ConversationID: "conv-1",
		ToolUse:        tu,
		Service:        "svc",
		Action:         "act",
		Host:           "h",
		Method:         "POST",
		Path:           "/p",
		Source:         ScopeDriftSourceTaskScope,
		ReasonText:     "no covering task",
	}

	stored, err := reg.Register(ctx, tmpl)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if stored.ID == "" {
		t.Fatal("Register: empty ID")
	}
	if stored.CreatedAt.IsZero() || stored.ExpiresAt.IsZero() {
		t.Fatal("Register: missing timestamps")
	}

	got, err := reg.Get(ctx, stored.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.AgentID != "agent-1" || got.Service != "svc" {
		t.Fatalf("Get returned wrong record: %+v", got)
	}

	// First claim succeeds; second claim is rejected by the one-shot cap.
	claimed, err := reg.ClaimOption(ctx, stored.ID, ScopeDriftOptionOneOff, "throwaway")
	if err != nil {
		t.Fatalf("ClaimOption: %v", err)
	}
	if claimed.ChosenOption != ScopeDriftOptionOneOff || claimed.Outcome != ScopeDriftOutcomePending || claimed.AgentNote != "throwaway" {
		t.Fatalf("ClaimOption result: %+v", claimed)
	}
	if _, err := reg.ClaimOption(ctx, stored.ID, ScopeDriftOptionExpand, ""); !errors.Is(err, ErrDriftAlreadyResolved) {
		t.Fatalf("second ClaimOption: want ErrDriftAlreadyResolved, got %v", err)
	}

	// SetOutcome(Succeeded) mints the pre-clear; LookupPreClear consumes it.
	if err := reg.SetOutcome(ctx, stored.ID, ScopeDriftOutcomeSucceeded); err != nil {
		t.Fatalf("SetOutcome: %v", err)
	}
	fp := stored.Fingerprint()
	driftID, ok := reg.LookupPreClear(ctx, "agent-1", fp)
	if !ok || driftID != stored.ID {
		t.Fatalf("LookupPreClear first hit: ok=%v id=%q want id=%q", ok, driftID, stored.ID)
	}
	// One-shot: second lookup misses.
	if _, ok := reg.LookupPreClear(ctx, "agent-1", fp); ok {
		t.Fatal("LookupPreClear second call: want miss (consumed), got hit")
	}
}

func TestScopeDriftRegistry_RollbackClaimClearsPreClear(t *testing.T) {
	// Regression: a SetOutcome(Succeeded) followed by a downstream
	// failure must roll back the pre-clear, not just the claim
	// fields. Otherwise LookupPreClear on the next turn treats the
	// failed flow as already-succeeded and skips drift handling.
	t.Parallel()
	ctx := context.Background()
	reg := NewMemoryScopeDriftRegistry(0)

	tu := conversation.ToolUse{ID: "tu-rollback", Name: "Bash", Input: []byte(`{"command":"x"}`)}
	stored, err := reg.Register(ctx, ScopeDrift{
		UserID: "u", AgentID: "agent-rb", ConversationID: "conv-rb",
		ToolUse: tu, Service: "svc", Action: "act", Host: "h",
		Method: "POST", Path: "/p", Source: ScopeDriftSourceTaskScope,
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, err := reg.ClaimOption(ctx, stored.ID, ScopeDriftOptionNewTask, ""); err != nil {
		t.Fatalf("ClaimOption: %v", err)
	}
	if err := reg.SetOutcome(ctx, stored.ID, ScopeDriftOutcomeSucceeded); err != nil {
		t.Fatalf("SetOutcome: %v", err)
	}

	// Simulate downstream failure: roll back the claim. Pre-clear
	// must go with it.
	if err := reg.RollbackClaim(ctx, stored.ID); err != nil {
		t.Fatalf("RollbackClaim: %v", err)
	}
	if _, ok := reg.LookupPreClear(ctx, "agent-rb", stored.Fingerprint()); ok {
		t.Fatal("LookupPreClear after RollbackClaim: want miss, got hit (stale pre-clear leaked past rollback)")
	}

	// Sanity: the claim itself was also reset.
	got, err := reg.Get(ctx, stored.ID)
	if err != nil {
		t.Fatalf("Get after rollback: %v", err)
	}
	if got.ChosenOption != "" || got.Outcome != "" || got.AgentNote != "" {
		t.Fatalf("RollbackClaim left claim fields populated: %+v", got)
	}
}

func TestScopeDriftRegistry_GetMissingIsNotFound(t *testing.T) {
	t.Parallel()
	reg := NewMemoryScopeDriftRegistry(0)
	if _, err := reg.Get(context.Background(), "drift-nope"); !errors.Is(err, ErrDriftNotFound) {
		t.Fatalf("Get unknown: want ErrDriftNotFound, got %v", err)
	}
}

func TestScopeDriftRegistry_ExpiredEntryPrunes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	reg := &memoryScopeDriftRegistry{
		ttl:     50 * time.Millisecond,
		now:     time.Now,
		drifts:  map[string]*ScopeDrift{},
		cleared: map[string]string{},
	}
	stored, err := reg.Register(ctx, ScopeDrift{
		UserID:         "u",
		AgentID:        "a",
		ConversationID: "c",
		ToolUse:        conversation.ToolUse{ID: "t"},
		Source:         ScopeDriftSourceTaskScope,
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	if _, err := reg.Get(ctx, stored.ID); !errors.Is(err, ErrDriftNotFound) {
		t.Fatalf("Get after TTL: want ErrDriftNotFound, got %v", err)
	}
}

func TestScopeDriftFingerprint_StableAndInputSensitive(t *testing.T) {
	t.Parallel()
	base := ScopeDrift{
		AgentID:        "a",
		ConversationID: "c",
		Service:        "svc",
		Action:         "act",
		Host:           "h",
		Method:         "POST",
		Path:           "/p",
		ToolUse:        conversation.ToolUse{Input: []byte(`{"x":1}`)},
	}
	other := base
	if base.Fingerprint() != other.Fingerprint() {
		t.Fatal("identical drifts must hash identically")
	}
	// Different input bytes ⇒ different fingerprint (security property).
	other.ToolUse.Input = []byte(`{"x":2}`)
	if base.Fingerprint() == other.Fingerprint() {
		t.Fatal("different input bytes must hash differently")
	}
	// Different conversation ⇒ different fingerprint.
	other = base
	other.ConversationID = "c2"
	if base.Fingerprint() == other.Fingerprint() {
		t.Fatal("different ConversationID must hash differently")
	}
}
