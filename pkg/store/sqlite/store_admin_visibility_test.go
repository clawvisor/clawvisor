package sqlite

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/clawvisor/clawvisor/pkg/store"
)

// TestCostSummaryIndexIsUsable: idx_llm_cost_time must index the same
// strftime('%s', timestamp) expression InstanceCostSummary filters on, so the
// planner uses the index instead of full-scanning. A plain-column index on
// timestamp is unusable for a strftime() predicate.
func TestCostSummaryIndexIsUsable(t *testing.T) {
	ctx := context.Background()
	db, err := New(ctx, filepath.Join(t.TempDir(), "clawvisor.db"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	rows, err := db.QueryContext(ctx,
		`EXPLAIN QUERY PLAN SELECT COUNT(*) FROM llm_request_cost
		 WHERE strftime('%s', timestamp) >= strftime('%s', ?)`, "2026-01-01T00:00:00Z")
	if err != nil {
		t.Fatalf("EXPLAIN QUERY PLAN: %v", err)
	}
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var a, b, c int
		var detail string
		if err := rows.Scan(&a, &b, &c, &detail); err != nil {
			t.Fatalf("scan plan: %v", err)
		}
		plan.WriteString(detail)
		plan.WriteByte('\n')
	}
	// Must be a SEARCH (index seek), not a SCAN. A plain-column index on
	// timestamp still shows up as "SCAN … USING COVERING INDEX idx_llm_cost_time"
	// — a full index scan — so asserting the index name alone would not catch
	// the regression. The expression index makes it a seek: "SEARCH … (<expr>>?)".
	got := plan.String()
	if !strings.Contains(got, "SEARCH") || !strings.Contains(got, "idx_llm_cost_time") {
		t.Fatalf("cost rollup does not seek idx_llm_cost_time (still full-scans):\n%s", got)
	}
}

func newAdminVisStore(t *testing.T) (*Store, context.Context) {
	t.Helper()
	ctx := context.Background()
	db, err := New(ctx, filepath.Join(t.TempDir(), "clawvisor.db"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return NewStore(db), ctx
}

// TestListAllAgents_IncludesInstanceRow: the admin fleet view returns agents
// across all owners INCLUDING the `_instance` (Terraform/CI) row, with
// owner_email joined; the user-scoped ListAgents still returns only the
// caller's own agents and never leaks the `_instance` row.
func TestListAllAgents_IncludesInstanceRow(t *testing.T) {
	st, ctx := newAdminVisStore(t)

	member, err := st.CreateUser(ctx, "member@test.example", "hash", store.RoleMember)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	memberAgent, err := st.CreateAgent(ctx, member.ID, "member-agent", "th-member")
	if err != nil {
		t.Fatalf("CreateAgent(member): %v", err)
	}
	// `_instance` is seeded by migration 060; attribute a Terraform agent to it.
	instAgent, err := st.CreateAgent(ctx, store.InstanceUserID, "terraform-agent", "th-inst")
	if err != nil {
		t.Fatalf("CreateAgent(_instance): %v", err)
	}

	all, err := st.ListAllAgents(ctx)
	if err != nil {
		t.Fatalf("ListAllAgents: %v", err)
	}
	byID := map[string]*store.Agent{}
	for _, a := range all {
		byID[a.ID] = a
	}
	if byID[memberAgent.ID] == nil || byID[instAgent.ID] == nil {
		t.Fatalf("ListAllAgents missing rows: member=%v instance=%v", byID[memberAgent.ID] != nil, byID[instAgent.ID] != nil)
	}
	if byID[memberAgent.ID].OwnerEmail != "member@test.example" {
		t.Fatalf("member owner_email = %q", byID[memberAgent.ID].OwnerEmail)
	}
	if byID[instAgent.ID].UserID != store.InstanceUserID {
		t.Fatalf("instance agent owner = %q, want _instance", byID[instAgent.ID].UserID)
	}

	// User-scoped ListAgents is unchanged: member sees only their own.
	memberOnly, err := st.ListAgents(ctx, member.ID)
	if err != nil {
		t.Fatalf("ListAgents(member): %v", err)
	}
	if len(memberOnly) != 1 || memberOnly[0].ID != memberAgent.ID {
		t.Fatalf("ListAgents(member) = %+v, want only member agent", memberOnly)
	}
	// The `_instance` row is absent from every user-scoped view except its own.
	instScoped, err := st.ListAgents(ctx, store.InstanceUserID)
	if err != nil {
		t.Fatalf("ListAgents(_instance): %v", err)
	}
	if len(instScoped) != 1 || instScoped[0].ID != instAgent.ID {
		t.Fatalf("ListAgents(_instance) = %+v", instScoped)
	}
}

// TestListAllAuditEvents_IncludesDeletedActor: a departed user's audit rows
// (user_id SET NULL on delete) remain in the admin cross-user view, attributed
// by the denormalized actor_email — offboarding visibility.
func TestListAllAuditEvents_IncludesDeletedActor(t *testing.T) {
	st, ctx := newAdminVisStore(t)

	user, err := st.CreateUser(ctx, "leaver@test.example", "hash", store.RoleMember)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	entry := &store.AuditEntry{
		UserID: user.ID, RequestID: "req-leaver", Timestamp: time.Now().UTC(),
		Service: "gmail", Action: "send", Decision: "allow", Outcome: "executed",
	}
	if err := st.LogAudit(ctx, entry); err != nil {
		t.Fatalf("LogAudit: %v", err)
	}
	if err := st.DeleteUser(ctx, user.ID); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}

	events, total, err := st.ListAllAuditEvents(ctx, store.AuditFilter{})
	if err != nil {
		t.Fatalf("ListAllAuditEvents: %v", err)
	}
	if total == 0 || len(events) == 0 {
		t.Fatal("deleted user's audit row dropped from admin view")
	}
	var found *store.AuditEntry
	for _, e := range events {
		if e.RequestID == "req-leaver" {
			found = e
		}
	}
	if found == nil {
		t.Fatal("departed actor's row not returned")
	}
	if found.UserID != "" {
		t.Fatalf("expected nulled user_id for departed actor, got %q", found.UserID)
	}
	if found.ActorEmail != "leaver@test.example" {
		t.Fatalf("actor_email = %q, want leaver@test.example (email-at-the-time)", found.ActorEmail)
	}
}

// TestInstanceCostSummary_AcrossUsers: spend rolls up across all users; the
// `_instance` (automation) row is included, attributed by actor_email, so
// Terraform/CI usage is not under-counted.
func TestInstanceCostSummary_AcrossUsers(t *testing.T) {
	st, ctx := newAdminVisStore(t)

	userA, _ := st.CreateUser(ctx, "a@test.example", "hash", store.RoleMember)
	userB, _ := st.CreateUser(ctx, "b@test.example", "hash", store.RoleMember)

	seed := func(userID, model string, micros int64) {
		auditID := "audit-" + userID + "-" + model + "-" + time.Now().Format("150405.000000000")
		e := &store.AuditEntry{
			ID: auditID, UserID: userID, RequestID: "req-" + auditID, Timestamp: time.Now().UTC(),
			Service: "llm", Action: "call", Decision: "allow", Outcome: "executed",
		}
		if err := st.LogAudit(ctx, e); err != nil {
			t.Fatalf("LogAudit: %v", err)
		}
		cm := micros
		if err := st.RecordLLMRequestCost(ctx, &store.LLMRequestCost{
			AuditID: auditID, UserID: userID, RequestID: e.RequestID, Timestamp: time.Now().UTC(),
			Provider: "anthropic", Model: model, InputTokens: 10, OutputTokens: 5, CostMicros: &cm,
		}); err != nil {
			t.Fatalf("RecordLLMRequestCost: %v", err)
		}
	}

	seed(userA.ID, "claude-opus-4-6", 1000)
	seed(userA.ID, "claude-opus-4-6", 500)
	seed(userB.ID, "claude-haiku-4-5", 250)
	seed(store.InstanceUserID, "claude-opus-4-6", 400)

	sum, err := st.InstanceCostSummary(ctx, store.InstanceCostWindowDaily)
	if err != nil {
		t.Fatalf("InstanceCostSummary: %v", err)
	}
	if sum.CostMicros != 2150 {
		t.Fatalf("total cost_micros = %d, want 2150", sum.CostMicros)
	}
	if sum.RequestCount != 4 {
		t.Fatalf("request_count = %d, want 4", sum.RequestCount)
	}
	if len(sum.ByUser) != 3 {
		t.Fatalf("by_user len = %d, want 3", len(sum.ByUser))
	}
	sawInstance := false
	for _, u := range sum.ByUser {
		if u.UserID == store.InstanceUserID {
			sawInstance = true
			if u.CostMicros != 400 {
				t.Fatalf("_instance spend = %d, want 400", u.CostMicros)
			}
		}
	}
	if !sawInstance {
		t.Fatal("_instance automation spend missing from rollup")
	}
}

// TestListAllPendingApprovals_AndGetByID: the admin approval queue spans all
// users with owner_email; GetPendingApprovalByID resolves a hold by primary
// key without a user scope.
func TestListAllPendingApprovals_AndGetByID(t *testing.T) {
	st, ctx := newAdminVisStore(t)

	member, _ := st.CreateUser(ctx, "holder@test.example", "hash", store.RoleMember)
	pa := &store.PendingApproval{
		UserID: member.ID, RequestID: "req-hold", AuditID: "audit-hold",
		RequestBlob: json.RawMessage(`{"service":"gmail"}`),
		ExpiresAt:   time.Now().Add(time.Hour).UTC(),
	}
	if err := st.SavePendingApproval(ctx, pa); err != nil {
		t.Fatalf("SavePendingApproval: %v", err)
	}

	all, err := st.ListAllPendingApprovals(ctx)
	if err != nil {
		t.Fatalf("ListAllPendingApprovals: %v", err)
	}
	if len(all) != 1 || all[0].RequestID != "req-hold" {
		t.Fatalf("ListAllPendingApprovals = %+v", all)
	}
	if all[0].OwnerEmail != "holder@test.example" {
		t.Fatalf("owner_email = %q", all[0].OwnerEmail)
	}

	got, err := st.GetPendingApprovalByID(ctx, pa.ID)
	if err != nil {
		t.Fatalf("GetPendingApprovalByID: %v", err)
	}
	if got.UserID != member.ID || got.OwnerEmail != "holder@test.example" {
		t.Fatalf("GetPendingApprovalByID = %+v", got)
	}

	if _, err := st.GetPendingApprovalByID(ctx, "nonexistent"); err != store.ErrNotFound {
		t.Fatalf("GetPendingApprovalByID(nonexistent) err = %v, want ErrNotFound", err)
	}
}

// TestPendingApprovals_ExcludeExpired: a hold whose expires_at is in the past
// must NOT appear in either the admin queue (ListAllPendingApprovals) or the
// user queue (ListPendingApprovals). Regression guard for the RFC3339-text vs
// datetime('now') comparison bug: expires_at is stored as "…T…Z", so a plain
// `expires_at > datetime('now')` compared the 'T' at char 10 against a space
// and treated any same-day expired hold as still live. The queries now compare
// via strftime('%s', …).
func TestPendingApprovals_ExcludeExpired(t *testing.T) {
	st, ctx := newAdminVisStore(t)

	member, _ := st.CreateUser(ctx, "holder@test.example", "hash", store.RoleMember)
	live := &store.PendingApproval{
		UserID: member.ID, RequestID: "req-live", AuditID: "audit-live",
		RequestBlob: json.RawMessage(`{"service":"gmail"}`),
		ExpiresAt:   time.Now().Add(time.Hour).UTC(),
	}
	expired := &store.PendingApproval{
		UserID: member.ID, RequestID: "req-expired", AuditID: "audit-expired",
		RequestBlob: json.RawMessage(`{"service":"gmail"}`),
		ExpiresAt:   time.Now().Add(-time.Minute).UTC(),
	}
	for _, pa := range []*store.PendingApproval{live, expired} {
		if err := st.SavePendingApproval(ctx, pa); err != nil {
			t.Fatalf("SavePendingApproval(%s): %v", pa.RequestID, err)
		}
	}

	assertOnlyLive := func(name string, list []*store.PendingApproval) {
		ids := map[string]bool{}
		for _, pa := range list {
			ids[pa.RequestID] = true
		}
		if !ids["req-live"] {
			t.Fatalf("%s: live hold missing", name)
		}
		if ids["req-expired"] {
			t.Fatalf("%s: expired hold still visible (RFC3339 comparison bug)", name)
		}
	}

	all, err := st.ListAllPendingApprovals(ctx)
	if err != nil {
		t.Fatalf("ListAllPendingApprovals: %v", err)
	}
	assertOnlyLive("ListAllPendingApprovals", all)

	perUser, err := st.ListPendingApprovals(ctx, member.ID)
	if err != nil {
		t.Fatalf("ListPendingApprovals: %v", err)
	}
	assertOnlyLive("ListPendingApprovals", perUser)
}
