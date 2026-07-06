package sqlite

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/clawvisor/clawvisor/pkg/store"
)

// TestAgentConversationAutoApproveThreshold_DefaultAndUpdate verifies
// the migration + scan + upsert path for the per-agent threshold
// column. Default at insert time is "off" (column DEFAULT in the
// migration); upsert round-trips the new value through both the
// per-agent SELECT path (GetAgentRuntimeSettings) and the list path
// (ListAgents → scanSQLiteAgentRuntimeSettings).
func TestAgentConversationAutoApproveThreshold_DefaultAndUpdate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, err := New(ctx, filepath.Join(t.TempDir(), "clawvisor.db"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := NewStore(db)

	user, err := st.CreateUser(ctx, "threshold@example.com", "hash", "")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	agent, err := st.CreateAgent(ctx, user.ID, "agent-x", "token-hash")
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}

	// Insert baseline runtime settings; threshold intentionally
	// omitted. The store's UpsertAgentRuntimeSettings normalizes ""
	// → "off" so the persisted value matches the migration's column
	// default and any future `== "off"` comparison elsewhere doesn't
	// have to defensively treat "" as equivalent.
	if err := st.UpsertAgentRuntimeSettings(ctx, &store.AgentRuntimeSettings{
		AgentID:                          agent.ID,
		RuntimeEnabled:                   true,
		RuntimeMode:                      "observe",
		StarterProfile:                   "none",
		OutboundCredentialMode:           "inherit",
		LiteProxySecretDetectionDisabled: true,
	}); err != nil {
		t.Fatalf("Upsert (baseline): %v", err)
	}
	got, err := st.GetAgentRuntimeSettings(ctx, agent.ID)
	if err != nil {
		t.Fatalf("GetAgentRuntimeSettings: %v", err)
	}
	if got.ConversationAutoApproveThreshold != store.ConversationAutoApproveOff {
		t.Errorf("baseline threshold = %q, want %q (upsert must normalize empty to 'off')",
			got.ConversationAutoApproveThreshold, store.ConversationAutoApproveOff)
	}

	// Set to medium and round-trip.
	if err := st.UpsertAgentRuntimeSettings(ctx, &store.AgentRuntimeSettings{
		AgentID:                          agent.ID,
		RuntimeEnabled:                   true,
		RuntimeMode:                      "observe",
		StarterProfile:                   "none",
		OutboundCredentialMode:           "inherit",
		LiteProxySecretDetectionDisabled: true,
		ConversationAutoApproveThreshold: store.ConversationAutoApproveMedium,
	}); err != nil {
		t.Fatalf("Upsert (medium): %v", err)
	}
	got, err = st.GetAgentRuntimeSettings(ctx, agent.ID)
	if err != nil {
		t.Fatalf("GetAgentRuntimeSettings: %v", err)
	}
	if got.ConversationAutoApproveThreshold != "medium" {
		t.Errorf("threshold = %q, want %q", got.ConversationAutoApproveThreshold, "medium")
	}

	// ListAgents (the second SELECT path) should also surface the
	// stored threshold — this catches scan-slot bugs.
	agents, err := st.ListAgents(ctx, user.ID)
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	if len(agents) != 1 || agents[0].RuntimeSettings == nil {
		t.Fatalf("expected one agent with runtime settings; got %+v", agents)
	}
	if agents[0].RuntimeSettings.ConversationAutoApproveThreshold != "medium" {
		t.Errorf("ListAgents threshold = %q, want %q",
			agents[0].RuntimeSettings.ConversationAutoApproveThreshold, "medium")
	}
}

// TestAgentConversationAutoApproveThreshold_FreshInsertDefault verifies
// the DEFAULT 'off' column default fires when a row is inserted without
// the column being explicitly listed — i.e. via a migration backfill or
// a future code path that writes only some fields. We do this by
// running a raw INSERT that omits the column.
func TestAgentConversationAutoApproveThreshold_FreshInsertDefault(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, err := New(ctx, filepath.Join(t.TempDir(), "clawvisor.db"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := NewStore(db)

	user, err := st.CreateUser(ctx, "raw@example.com", "hash", "")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	agent, err := st.CreateAgent(ctx, user.ID, "agent-y", "token-hash-y")
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	// Insert raw with the threshold column omitted — exercise the
	// column DEFAULT 'off'.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO agent_runtime_settings (
			agent_id, runtime_enabled, runtime_mode, starter_profile,
			outbound_credential_mode, inject_stored_bearer, lite_proxy_secret_detection_disabled
		) VALUES (?, 1, 'observe', 'none', 'inherit', 0, 1)
	`, agent.ID); err != nil {
		t.Fatalf("raw INSERT: %v", err)
	}
	got, err := st.GetAgentRuntimeSettings(ctx, agent.ID)
	if err != nil {
		t.Fatalf("GetAgentRuntimeSettings: %v", err)
	}
	if got.ConversationAutoApproveThreshold != "off" {
		t.Errorf("column DEFAULT failed: threshold = %q, want %q",
			got.ConversationAutoApproveThreshold, "off")
	}
}
