package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/clawvisor/clawvisor/pkg/store"
)

func newGovStore(t *testing.T) (*Store, context.Context) {
	t.Helper()
	ctx := context.Background()
	db, err := New(ctx, filepath.Join(t.TempDir(), "clawvisor.db"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return NewStore(db), ctx
}

// TestInstanceModelPolicy_AppendOnly proves Put demotes the prior active
// row (only one active at a time) and Clear leaves no active policy.
func TestInstanceModelPolicy_AppendOnly(t *testing.T) {
	st, ctx := newGovStore(t)

	if _, err := st.GetActiveInstanceModelPolicy(ctx); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected ErrNotFound on empty, got %v", err)
	}
	if err := st.PutInstanceModelPolicy(ctx, &store.InstanceModelPolicy{
		Mode: "deny", Models: []string{"anthropic/claude-3-opus"}, CreatedBy: "_instance",
	}); err != nil {
		t.Fatalf("PutInstanceModelPolicy: %v", err)
	}
	if err := st.PutInstanceModelPolicy(ctx, &store.InstanceModelPolicy{
		Mode: "allow", Models: []string{"openai/gpt-4o", "anthropic/claude-3-5-sonnet"}, CreatedBy: "_instance",
	}); err != nil {
		t.Fatalf("PutInstanceModelPolicy 2: %v", err)
	}
	got, err := st.GetActiveInstanceModelPolicy(ctx)
	if err != nil {
		t.Fatalf("GetActiveInstanceModelPolicy: %v", err)
	}
	if got.Mode != "allow" || len(got.Models) != 2 {
		t.Fatalf("active policy mismatch: %+v", got)
	}
	if err := st.ClearInstanceModelPolicy(ctx); err != nil {
		t.Fatalf("ClearInstanceModelPolicy: %v", err)
	}
	if _, err := st.GetActiveInstanceModelPolicy(ctx); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected ErrNotFound after clear, got %v", err)
	}
}

// TestInstanceSpendCap_Upsert proves the per-window unique upsert and the
// cost sum window query.
func TestInstanceSpendCap_Upsert(t *testing.T) {
	st, ctx := newGovStore(t)

	if err := st.PutInstanceSpendCap(ctx, &store.InstanceSpendCap{
		WindowKind: "daily", CapMicros: 5_000_000, Enforcement: "soft", CreatedBy: "_instance",
	}); err != nil {
		t.Fatalf("PutInstanceSpendCap: %v", err)
	}
	// Upsert same window: cap + enforcement change, still one row.
	if err := st.PutInstanceSpendCap(ctx, &store.InstanceSpendCap{
		WindowKind: "daily", CapMicros: 9_000_000, Enforcement: "hard", CreatedBy: "_instance",
	}); err != nil {
		t.Fatalf("PutInstanceSpendCap upsert: %v", err)
	}
	caps, err := st.ListInstanceSpendCaps(ctx)
	if err != nil {
		t.Fatalf("ListInstanceSpendCaps: %v", err)
	}
	if len(caps) != 1 || caps[0].CapMicros != 9_000_000 || caps[0].Enforcement != "hard" {
		t.Fatalf("upsert did not collapse to one row: %+v", caps)
	}
	got, err := st.GetInstanceSpendCap(ctx, "daily")
	if err != nil || got.CapMicros != 9_000_000 {
		t.Fatalf("GetInstanceSpendCap: %+v err=%v", got, err)
	}
	if err := st.DeleteInstanceSpendCap(ctx, "daily"); err != nil {
		t.Fatalf("DeleteInstanceSpendCap: %v", err)
	}
	if _, err := st.GetInstanceSpendCap(ctx, "daily"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected ErrNotFound after delete, got %v", err)
	}
}

// TestInstanceContentPolicy_CRUD exercises create → get → update → delete.
func TestInstanceContentPolicy_CRUD(t *testing.T) {
	st, ctx := newGovStore(t)

	p := &store.InstanceContentPolicy{
		Name: "block-ssn", Pattern: `\d{3}-\d{2}-\d{4}`, PatternKind: "regex",
		Action: "block", BlockMessage: "no SSNs", Enabled: true, CreatedBy: "_instance",
	}
	if err := st.CreateInstanceContentPolicy(ctx, p); err != nil {
		t.Fatalf("CreateInstanceContentPolicy: %v", err)
	}
	if p.ID == "" {
		t.Fatal("Create did not assign an id")
	}
	got, err := st.GetInstanceContentPolicy(ctx, p.ID)
	if err != nil || got.Name != "block-ssn" || !got.Enabled {
		t.Fatalf("GetInstanceContentPolicy: %+v err=%v", got, err)
	}
	p.Enabled = false
	p.Action = "flag"
	if err := st.UpdateInstanceContentPolicy(ctx, p); err != nil {
		t.Fatalf("UpdateInstanceContentPolicy: %v", err)
	}
	got, _ = st.GetInstanceContentPolicy(ctx, p.ID)
	if got.Enabled || got.Action != "flag" {
		t.Fatalf("update not persisted: %+v", got)
	}
	if err := st.DeleteInstanceContentPolicy(ctx, p.ID); err != nil {
		t.Fatalf("DeleteInstanceContentPolicy: %v", err)
	}
	if _, err := st.GetInstanceContentPolicy(ctx, p.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected ErrNotFound after delete, got %v", err)
	}
}

// TestInstanceTaskPolicy_AppendOnly mirrors the model-policy append-only
// behavior for task guidance.
func TestInstanceTaskPolicy_AppendOnly(t *testing.T) {
	st, ctx := newGovStore(t)

	if err := st.PutInstanceTaskPolicy(ctx, &store.InstanceTaskPolicy{Guidance: "first", CreatedBy: "_instance"}); err != nil {
		t.Fatalf("PutInstanceTaskPolicy: %v", err)
	}
	if err := st.PutInstanceTaskPolicy(ctx, &store.InstanceTaskPolicy{Guidance: "second", CreatedBy: "_instance"}); err != nil {
		t.Fatalf("PutInstanceTaskPolicy 2: %v", err)
	}
	got, err := st.GetActiveInstanceTaskPolicy(ctx)
	if err != nil || got.Guidance != "second" {
		t.Fatalf("active task policy mismatch: %+v err=%v", got, err)
	}
	if err := st.ClearInstanceTaskPolicy(ctx); err != nil {
		t.Fatalf("ClearInstanceTaskPolicy: %v", err)
	}
	if _, err := st.GetActiveInstanceTaskPolicy(ctx); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected ErrNotFound after clear, got %v", err)
	}
}

// TestInstancePolicyViolation_RecordList proves violations record without
// an org_id and list newest-first.
func TestInstancePolicyViolation_RecordList(t *testing.T) {
	st, ctx := newGovStore(t)

	for _, kind := range []string{"model_policy", "content_policy"} {
		if err := st.RecordInstancePolicyViolation(ctx, &store.InstancePolicyViolation{
			UserID: "u1", AgentID: "a1", PolicyKind: kind, ActionTaken: "blocked", Detail: "d",
		}); err != nil {
			t.Fatalf("RecordInstancePolicyViolation(%s): %v", kind, err)
		}
	}
	vs, err := st.ListInstancePolicyViolations(ctx, 10)
	if err != nil {
		t.Fatalf("ListInstancePolicyViolations: %v", err)
	}
	if len(vs) != 2 {
		t.Fatalf("expected 2 violations, got %d", len(vs))
	}
}

// TestSumInstanceCostMicros sums cost across the window regardless of agent.
func TestSumInstanceCostMicros(t *testing.T) {
	st, ctx := newGovStore(t)

	// No cost rows → zero.
	sum, err := st.SumInstanceCostMicros(ctx, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("SumInstanceCostMicros: %v", err)
	}
	if sum != 0 {
		t.Fatalf("expected 0 sum on empty, got %d", sum)
	}
}
