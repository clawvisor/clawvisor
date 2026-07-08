package govlocal

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/clawvisor/clawvisor/pkg/store"
	"github.com/clawvisor/clawvisor/pkg/store/sqlite"
)

func newStore(t *testing.T) (store.Store, context.Context) {
	t.Helper()
	ctx := context.Background()
	db, err := sqlite.New(ctx, filepath.Join(t.TempDir(), "clawvisor.db"))
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return sqlite.NewStore(db), ctx
}

var seedCounter int

// seedCost inserts one llm_request_cost row with the given cost_micros in
// the current daily window. It bypasses the audit_id FK (there is no audit
// row in a unit test) by toggling foreign_keys off for the single insert —
// the SQLite store uses one connection, so the pragma is scoped to it.
func seedCost(t *testing.T, st store.Store, ctx context.Context, micros int64) {
	t.Helper()
	db := st.(interface{ DB() *sql.DB }).DB()
	if _, err := db.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
		t.Fatalf("pragma off: %v", err)
	}
	defer db.ExecContext(ctx, `PRAGMA foreign_keys = ON`)
	seedCounter++
	_, err := db.ExecContext(ctx, `
		INSERT INTO llm_request_cost (audit_id, user_id, request_id, timestamp, provider, model, cost_micros)
		VALUES (?, 'u1', ?, ?, 'anthropic', 'claude', ?)`,
		fmt.Sprintf("seed-%d", seedCounter), fmt.Sprintf("req-%d", seedCounter),
		time.Now().UTC().Format(time.RFC3339), micros)
	if err != nil {
		t.Fatalf("seed cost: %v", err)
	}
}

// TestEvaluateCap_WarningLevels verifies the "80"/"100" warning math and
// hard-vs-soft blocking, matching cloud's evaluateCap exactly.
func TestEvaluateCap_WarningLevels(t *testing.T) {
	cases := []struct {
		name        string
		current     int64
		cap         int64
		enforcement string
		wantAllow   bool
		wantWarning string
	}{
		{"under-80-soft", 700_000, 1_000_000, "soft", true, ""},
		{"at-80-soft", 800_000, 1_000_000, "soft", true, "80"},
		{"at-80-hard", 850_000, 1_000_000, "hard", true, "80"},
		{"at-100-soft", 1_000_000, 1_000_000, "soft", true, "100"},
		{"over-100-soft", 1_500_000, 1_000_000, "soft", true, "100"},
		{"at-100-hard", 1_000_000, 1_000_000, "hard", false, "100"},
		{"over-100-hard", 2_000_000, 1_000_000, "hard", false, "100"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := evaluateCap(tc.current, tc.cap, tc.enforcement, "daily")
			if v.allow != tc.wantAllow || v.warning != tc.wantWarning {
				t.Fatalf("evaluateCap = (allow=%v warning=%q), want (allow=%v warning=%q)",
					v.allow, v.warning, tc.wantAllow, tc.wantWarning)
			}
		})
	}
}

// TestMerge_StrictestWins proves a hard block beats a soft warning and the
// highest warning wins among allows.
func TestMerge_StrictestWins(t *testing.T) {
	soft80 := capVerdict{allow: true, warning: "80", reason: "r80"}
	hard100 := capVerdict{allow: false, warning: "100", reason: "block"}
	soft100 := capVerdict{allow: true, warning: "100", reason: "r100"}

	if m := soft80.merge(hard100); m.allow {
		t.Fatal("hard block must win over soft warning")
	}
	if m := (capVerdict{allow: true}).merge(soft80).merge(soft100); m.warning != "100" {
		t.Fatalf("highest warning should win, got %q", m.warning)
	}
}

// TestBuildCheckModelPolicy_DenyAllow exercises the deny/allow modes and
// the empty-policy fall-through against a real store.
func TestBuildCheckModelPolicy_DenyAllow(t *testing.T) {
	st, ctx := newStore(t)
	check := BuildCheckModelPolicy(st)

	// No policy → allow.
	if allow, _ := check(ctx, LocalOrgID, "anthropic/claude-3-opus"); !allow {
		t.Fatal("no policy should allow")
	}
	// Deny list.
	if err := st.PutInstanceModelPolicy(ctx, &store.InstanceModelPolicy{
		Mode: "deny", Models: []string{"anthropic/claude-3-opus"}, CreatedBy: "_instance",
	}); err != nil {
		t.Fatal(err)
	}
	if allow, reason := check(ctx, LocalOrgID, "anthropic/claude-3-opus"); allow || reason == "" {
		t.Fatalf("deny list should block with a reason; allow=%v reason=%q", allow, reason)
	}
	if allow, _ := check(ctx, LocalOrgID, "openai/gpt-4o"); !allow {
		t.Fatal("model not on deny list should allow")
	}
	// Allow list.
	if err := st.PutInstanceModelPolicy(ctx, &store.InstanceModelPolicy{
		Mode: "allow", Models: []string{"openai/gpt-4o"}, CreatedBy: "_instance",
	}); err != nil {
		t.Fatal(err)
	}
	if allow, _ := check(ctx, LocalOrgID, "openai/gpt-4o"); !allow {
		t.Fatal("allow-listed model should allow")
	}
	if allow, _ := check(ctx, LocalOrgID, "anthropic/claude-3-opus"); allow {
		t.Fatal("model not on allow list should block")
	}
}

// TestBuildScanContentPolicy_BlockAndFlag proves the first block wins and
// flag matches accumulate names.
func TestBuildScanContentPolicy_BlockAndFlag(t *testing.T) {
	st, ctx := newStore(t)
	if err := st.CreateInstanceContentPolicy(ctx, &store.InstanceContentPolicy{
		Name: "flag-secret", Pattern: "secret", PatternKind: "keyword", Action: "flag", Enabled: true, CreatedBy: "_instance",
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateInstanceContentPolicy(ctx, &store.InstanceContentPolicy{
		Name: "block-ssn", Pattern: `\d{3}-\d{2}-\d{4}`, PatternKind: "regex", Action: "block",
		BlockMessage: "no SSNs allowed", Enabled: true, CreatedBy: "_instance",
	}); err != nil {
		t.Fatal(err)
	}
	scan := BuildScanContentPolicy(st)

	// Flag only.
	allow, msg, _, flagged := scan(ctx, LocalOrgID, "this is secret info")
	if !allow || msg != "" || len(flagged) != 1 || flagged[0] != "flag-secret" {
		t.Fatalf("flag path mismatch: allow=%v msg=%q flagged=%v", allow, msg, flagged)
	}
	// Block wins.
	allow, msg, _, _ = scan(ctx, LocalOrgID, "ssn 123-45-6789 and secret")
	if allow || msg != "no SSNs allowed" {
		t.Fatalf("block path mismatch: allow=%v msg=%q", allow, msg)
	}
}

// TestBuildCheckSpendCap_SoftWarnHardBlock seeds cost rows and checks the
// warning-level / block verdicts end to end via the store.
func TestBuildCheckSpendCap_SoftWarnHardBlock(t *testing.T) {
	st, ctx := newStore(t)
	// Soft daily cap of $1 (1,000,000 micros).
	if err := st.PutInstanceSpendCap(ctx, &store.InstanceSpendCap{
		WindowKind: "daily", CapMicros: 1_000_000, Enforcement: "soft", CreatedBy: "_instance",
	}); err != nil {
		t.Fatal(err)
	}
	// No spend → allow, no warning.
	check := BuildCheckSpendCap(st)
	if allow, warn, _ := check(ctx, LocalOrgID, "agent-1"); !allow || warn != "" {
		t.Fatalf("empty spend should allow with no warning; allow=%v warn=%q", allow, warn)
	}

	// Seed 90% of cap → fresh cache recompute (new cap builder) sees "80".
	seedCost(t, st, ctx, 900_000)
	check2 := BuildCheckSpendCap(st)
	if allow, warn, reason := check2(ctx, LocalOrgID, "agent-1"); !allow || warn != "80" || reason == "" {
		t.Fatalf("90%% of soft cap should warn 80; allow=%v warn=%q reason=%q", allow, warn, reason)
	}

	// Switch to hard cap and push past 100%.
	if err := st.PutInstanceSpendCap(ctx, &store.InstanceSpendCap{
		WindowKind: "daily", CapMicros: 1_000_000, Enforcement: "hard", CreatedBy: "_instance",
	}); err != nil {
		t.Fatal(err)
	}
	seedCost(t, st, ctx, 200_000) // total 1,100,000 > cap
	check3 := BuildCheckSpendCap(st)
	if allow, warn, _ := check3(ctx, LocalOrgID, "agent-1"); allow || warn != "100" {
		t.Fatalf("hard cap over 100%% should block; allow=%v warn=%q", allow, warn)
	}
}
