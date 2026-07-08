package clawvisor

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/clawvisor/clawvisor/internal/govlocal"
	"github.com/clawvisor/clawvisor/pkg/store"
	"github.com/clawvisor/clawvisor/pkg/store/sqlite"
)

func govTestStore(t *testing.T) store.Store {
	t.Helper()
	ctx := context.Background()
	db, err := sqlite.New(ctx, filepath.Join(t.TempDir(), "clawvisor.db"))
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return sqlite.NewStore(db)
}

// TestLocalGovCloudPrecedence proves per-hook precedence: a cloud callback
// wins for the hook it populates, while govlocal fills every other hook. A
// cloud CheckModelPolicy is provided; the local spend cap must still fire
// (from govlocal), and the cloud model hook must be the one wired.
func TestLocalGovCloudPrecedence(t *testing.T) {
	st := govTestStore(t)
	ctx := context.Background()

	// Local spend cap so govlocal's CheckSpendCap has something to enforce.
	if err := st.PutInstanceSpendCap(ctx, &store.InstanceSpendCap{
		WindowKind: "daily", CapMicros: 1_000_000, Enforcement: "hard", CreatedBy: "_instance",
	}); err != nil {
		t.Fatal(err)
	}

	cloudModelCalled := false
	cloud := &OrgGovOptions{
		CheckModelPolicy: func(ctx context.Context, orgID, model string) (bool, string) {
			cloudModelCalled = true
			return false, "cloud says no"
		},
		// OrgIDForAgent provided → the "local" sentinel must NOT be installed.
		OrgIDForAgent: func(ctx context.Context, agentID string) string { return "org-123" },
	}

	callbacks, orgIDForAgent, wire := composeGovernanceCallbacks(cloud, st, true)
	if !wire {
		t.Fatal("expected governance to be wired")
	}

	// Cloud model hook wins.
	if allow, reason := callbacks.CheckModelPolicy(ctx, "org-123", "anthropic/claude-3-opus"); allow || reason != "cloud says no" {
		t.Fatalf("cloud CheckModelPolicy should win; allow=%v reason=%q", allow, reason)
	}
	if !cloudModelCalled {
		t.Fatal("cloud CheckModelPolicy was not invoked")
	}

	// Local spend cap still fires (govlocal filled the nil cloud hook).
	if callbacks.CheckSpendCap == nil {
		t.Fatal("local CheckSpendCap should fill the nil cloud hook")
	}
	if callbacks.ScanContentPolicy == nil || callbacks.RecordViolation == nil {
		t.Fatal("govlocal should fill the remaining nil hooks")
	}

	// The cloud resolver is preserved; the "local" sentinel is NOT installed.
	if got := orgIDForAgent(ctx, "agent-1"); got != "org-123" {
		t.Fatalf("cloud OrgIDForAgent must win; got %q (sentinel leak?)", got)
	}
}

// TestLocalGovPureOSSInstallsSentinel proves that with no cloud layer, every
// hook comes from govlocal and the "local" OrgID shim is installed so the
// instance-scoped callbacks actually fire.
func TestLocalGovPureOSSInstallsSentinel(t *testing.T) {
	st := govTestStore(t)
	callbacks, orgIDForAgent, wire := composeGovernanceCallbacks(nil, st, true)
	if !wire || callbacks.CheckModelPolicy == nil || callbacks.CheckSpendCap == nil ||
		callbacks.ScanContentPolicy == nil || callbacks.RecordViolation == nil {
		t.Fatal("pure-OSS build should wire all four local hooks")
	}
	if got := orgIDForAgent(context.Background(), "agent-1"); got != govlocal.LocalOrgID {
		t.Fatalf("pure-OSS build should install the %q sentinel; got %q", govlocal.LocalOrgID, got)
	}
}

// TestLocalGovDisabledComposesNothing proves governance disabled produces no
// callbacks at all (wire=false), regardless of the store.
func TestLocalGovDisabledComposesNothing(t *testing.T) {
	st := govTestStore(t)
	callbacks, _, wire := composeGovernanceCallbacks(nil, st, false)
	if wire || callbacks.CheckModelPolicy != nil {
		t.Fatal("disabled governance must compose no callbacks")
	}
}
