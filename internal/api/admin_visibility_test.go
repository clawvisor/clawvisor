package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/clawvisor/clawvisor/pkg/config"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// ── 04b admin-visibility helpers ────────────────────────────────────────────

// setRole forces a user's role in the store. RequireAdmin reloads the role
// from the DB on every request, so an existing access token picks it up.
func setRole(t *testing.T, env *testEnv, userID, role string) {
	t.Helper()
	if err := env.Store.UpdateUserRole(context.Background(), userID, role); err != nil {
		t.Fatalf("UpdateUserRole(%s,%s): %v", userID, role, err)
	}
}

// mintInstanceAdminToken mints a cvat_ instance-admin token via the admin's
// JWT and returns the plaintext value. Used to create `_instance`-owned
// (Terraform/CI) resources.
func mintInstanceAdminToken(t *testing.T, env *testEnv, adminToken string) string {
	t.Helper()
	resp := env.do("POST", "/api/tokens", adminToken, map[string]any{"name": "tf", "scope": "instance-admin"})
	body := mustStatus(t, resp, http.StatusCreated)
	return str(t, body, "token")
}

// raiseHold makes sc's agent raise a request-level approval hold for
// (service,"run") and returns the pending request_id. The task is approved
// with auto_execute=false so the follow-up gateway request is held.
func raiseHold(t *testing.T, env *testEnv, sc *scenario, service string) string {
	t.Helper()
	taskID := sc.createApprovedTask(t, env, service, "run", false)
	reqID := "req-hold-" + randSuffix()
	result := sc.gatewayRequestWithTask(env, reqID, service, "run", taskID)
	if result["status"] != "pending" {
		t.Fatalf("expected pending hold, got %v", result["status"])
	}
	return reqID
}

// adminPendingIDForRequest lists the admin approval queue and returns the
// pending-approval id whose request_id matches reqID.
func adminPendingIDForRequest(t *testing.T, env *testEnv, adminToken, reqID string) string {
	t.Helper()
	resp := env.do("GET", "/api/admin/approvals", adminToken, nil)
	body := mustStatus(t, resp, http.StatusOK)
	for _, e := range arr(t, body, "entries") {
		m, _ := e.(map[string]any)
		if m == nil {
			continue
		}
		if m["request_id"] == reqID {
			return str(t, m, "id")
		}
	}
	t.Fatalf("request %s not present in admin approval queue: %+v", reqID, body)
	return ""
}

// ── Tests ───────────────────────────────────────────────────────────────────

// TestAdminSeesAllAgents: two members create agents + one `_instance` (token)
// agent; admin GET /api/admin/agents returns all three with owner attribution;
// a member sees only their own via the normal route.
func TestAdminSeesAllAgents(t *testing.T) {
	env := newTestEnv(t)

	admin := newSession(t, env)
	setRole(t, env, admin.UserID, store.RoleAdmin)

	memberA := newScenario(t, env, "a")
	memberB := newScenario(t, env, "b")
	setRole(t, env, memberA.session.UserID, store.RoleMember)
	setRole(t, env, memberB.session.UserID, store.RoleMember)

	// `_instance`-owned agent via an instance-admin token.
	tok := mintInstanceAdminToken(t, env, admin.AccessToken)
	var tokenAgent struct {
		ID string `json:"id"`
	}
	resp := env.do("POST", "/api/agents", tok, map[string]any{"name": "terraform-agent"})
	decode(t, resp, &tokenAgent)
	if tokenAgent.ID == "" {
		t.Fatal("token-created agent has no id")
	}

	// Admin fleet view returns all three, with owner attribution.
	resp = env.do("GET", "/api/admin/agents", admin.AccessToken, nil)
	body := mustStatus(t, resp, http.StatusOK)
	agents := arr(t, body, "agents")
	if len(agents) != 3 {
		t.Fatalf("admin fleet view len = %d, want 3: %+v", len(agents), agents)
	}

	sawA, sawB, sawInstance := false, false, false
	for _, a := range agents {
		m := a.(map[string]any)
		id, _ := m["id"].(string)
		userID, _ := m["user_id"].(string)
		ownerLabel, _ := m["owner_label"].(string)
		if ownerLabel == "" {
			t.Fatalf("agent %s missing owner_label", id)
		}
		switch id {
		case memberA.AgentID:
			sawA = true
			if ownerLabel != memberA.session.Email {
				t.Fatalf("member A owner_label = %q, want %q", ownerLabel, memberA.session.Email)
			}
		case memberB.AgentID:
			sawB = true
		case tokenAgent.ID:
			sawInstance = true
			if userID != store.InstanceUserID {
				t.Fatalf("token agent owner = %q, want _instance", userID)
			}
			if ownerLabel != "Terraform / automation" {
				t.Fatalf("_instance owner_label = %q, want automation label", ownerLabel)
			}
		}
	}
	if !sawA || !sawB || !sawInstance {
		t.Fatalf("fleet view missing an agent: A=%v B=%v instance=%v", sawA, sawB, sawInstance)
	}

	// A member sees ONLY their own agent via the normal, user-scoped route.
	var memberList []map[string]any
	resp = env.do("GET", "/api/agents", memberA.session.AccessToken, nil)
	decode(t, resp, &memberList)
	if len(memberList) != 1 || memberList[0]["id"] != memberA.AgentID {
		t.Fatalf("member self-view = %+v, want only own agent %s", memberList, memberA.AgentID)
	}
	// The `_instance` agent is visible ONLY in the admin view, nowhere else.
	for _, a := range memberList {
		if a["id"] == tokenAgent.ID {
			t.Fatal("_instance agent leaked into a member's self-view")
		}
	}
}

// TestMemberForbiddenOnAdminRoutes: a member gets 403 on every /api/admin/*.
func TestMemberForbiddenOnAdminRoutes(t *testing.T) {
	env := newTestEnv(t)
	member := newSession(t, env)
	setRole(t, env, member.UserID, store.RoleMember)

	routes := []struct {
		method, path string
	}{
		{"GET", "/api/admin/agents"},
		{"GET", "/api/admin/approvals"},
		{"POST", "/api/admin/approvals/does-not-exist/resolve"},
		{"GET", "/api/admin/audit"},
		{"GET", "/api/admin/costs?window=daily"},
	}
	for _, r := range routes {
		var body any
		if r.method == "POST" {
			body = map[string]any{"decision": "approve"}
		}
		resp := env.do(r.method, r.path, member.AccessToken, body)
		got := mustStatus(t, resp, http.StatusForbidden)
		if got["code"] != "FORBIDDEN" {
			t.Fatalf("%s %s code = %v, want FORBIDDEN", r.method, r.path, got["code"])
		}
	}
}

// TestAdminResolvesMemberHold: a member's agent raises a hold; an admin
// resolves it via /api/admin/approvals/{id}/resolve; the member's request
// unblocks (the hold moves out of pending). This is AGENT-GUIDE §2B's
// acceptance narrative: member triggers, ADMIN resolves.
func TestAdminResolvesMemberHold(t *testing.T) {
	adapter := newMockAdapter("mock.adminresolve", "run").withResult("ok", map[string]any{"done": true})
	env := newTestEnv(t, adapter)

	admin := newSession(t, env)
	setRole(t, env, admin.UserID, store.RoleAdmin)
	member := newScenario(t, env, "m")
	setRole(t, env, member.session.UserID, store.RoleMember)

	reqID := raiseHold(t, env, member, "mock.adminresolve")

	// The admin sees the member's hold in the queue with owner attribution.
	id := adminPendingIDForRequest(t, env, admin.AccessToken, reqID)

	resp := env.do("POST", "/api/admin/approvals/"+id+"/resolve", admin.AccessToken, map[string]any{"decision": "approve"})
	res := mustStatus(t, resp, http.StatusOK)
	if res["status"] != "resolved" || res["decision"] != "approve" {
		t.Fatalf("admin resolve response = %+v", res)
	}

	// The hold is no longer pending: the member's own queue is empty and the
	// canonical row moved to approved.
	pa, err := env.Store.GetPendingApprovalByID(context.Background(), id)
	if err != nil {
		t.Fatalf("GetPendingApprovalByID: %v", err)
	}
	if pa.Status != "approved" {
		t.Fatalf("hold status = %q, want approved (unblocked)", pa.Status)
	}

	var memberQueue struct {
		Total int `json:"total"`
	}
	resp = env.do("GET", "/api/approvals", member.session.AccessToken, nil)
	decode(t, resp, &memberQueue)
	if memberQueue.Total != 0 {
		t.Fatalf("member queue total = %d, want 0 after admin resolve", memberQueue.Total)
	}
}

// TestSelfApproveDisabled: with allow_self_approve=false, the member cannot
// resolve their own hold (403 via the member route); a different user (the
// admin, via the admin route) can.
func TestSelfApproveDisabled(t *testing.T) {
	adapter := newMockAdapter("mock.selfapprove", "run").withResult("ok", map[string]any{"done": true})
	env := newTestEnvWithConfig(t, config.LLMConfig{}, nil, func(c *config.Config) {
		c.Approval.AllowSelfApprove = false
	}, adapter)

	admin := newSession(t, env)
	setRole(t, env, admin.UserID, store.RoleAdmin)
	member := newScenario(t, env, "m")
	setRole(t, env, member.session.UserID, store.RoleMember)

	reqID := raiseHold(t, env, member, "mock.selfapprove")

	// The member cannot resolve their OWN hold via the member route.
	resp := member.session.do("POST", "/api/approvals/"+reqID+"/approve", map[string]any{"resolution": "allow_once"})
	got := mustStatus(t, resp, http.StatusForbidden)
	if got["code"] != "SELF_APPROVE_FORBIDDEN" {
		t.Fatalf("member self-approve code = %v, want SELF_APPROVE_FORBIDDEN", got["code"])
	}

	// A different user (the admin) can resolve it via the admin route.
	id := adminPendingIDForRequest(t, env, admin.AccessToken, reqID)
	resp = env.do("POST", "/api/admin/approvals/"+id+"/resolve", admin.AccessToken, map[string]any{"decision": "approve"})
	mustStatus(t, resp, http.StatusOK)
}

// TestAdminCannotSelfApprove (F7): with allow_self_approve=false, an admin
// cannot resolve a hold raised by their OWN agent (403), but can resolve
// another user's; on a solo-admin instance the self-resolve is permitted and
// written to audit as a self-approval.
func TestAdminCannotSelfApprove(t *testing.T) {
	adapter := newMockAdapter("mock.adminself", "run").withResult("ok", map[string]any{"done": true})
	adapter2 := newMockAdapter("mock.adminself2", "run").withResult("ok", map[string]any{"done": true})
	env := newTestEnvWithConfig(t, config.LLMConfig{}, nil, func(c *config.Config) {
		c.Approval.AllowSelfApprove = false
	}, adapter, adapter2)

	// Two admins so the solo-admin exception does NOT apply.
	adminA := newScenario(t, env, "adminA")
	adminB := newSession(t, env)
	setRole(t, env, adminA.session.UserID, store.RoleAdmin)
	setRole(t, env, adminB.UserID, store.RoleAdmin)

	// adminA's own agent raises a hold; adminA must not self-resolve.
	reqID := raiseHold(t, env, adminA, "mock.adminself")
	id := adminPendingIDForRequest(t, env, adminA.session.AccessToken, reqID)

	resp := env.do("POST", "/api/admin/approvals/"+id+"/resolve", adminA.session.AccessToken, map[string]any{"decision": "approve"})
	got := mustStatus(t, resp, http.StatusForbidden)
	if got["code"] != "SELF_APPROVE_FORBIDDEN" {
		t.Fatalf("admin self-approve code = %v, want SELF_APPROVE_FORBIDDEN", got["code"])
	}

	// A DIFFERENT admin can resolve it.
	resp = env.do("POST", "/api/admin/approvals/"+id+"/resolve", adminB.AccessToken, map[string]any{"decision": "approve"})
	mustStatus(t, resp, http.StatusOK)

	// Solo-admin instance: demote adminB to member so adminA is the only
	// admin, then adminA may self-resolve — but it is logged as a
	// self-approval in the audit trail.
	setRole(t, env, adminB.UserID, store.RoleMember)
	soloReq := raiseHold(t, env, adminA, "mock.adminself2")
	soloID := adminPendingIDForRequest(t, env, adminA.session.AccessToken, soloReq)
	resp = env.do("POST", "/api/admin/approvals/"+soloID+"/resolve", adminA.session.AccessToken, map[string]any{"decision": "approve"})
	mustStatus(t, resp, http.StatusOK)

	// The self-approval is visible in the admin audit view.
	resp = env.do("GET", "/api/admin/audit?service=governance", adminA.session.AccessToken, nil)
	auditBody := mustStatus(t, resp, http.StatusOK)
	sawSelfApproval := false
	for _, e := range arr(t, auditBody, "entries") {
		m := e.(map[string]any)
		if m["action"] == "self_approve" && m["outcome"] == "self_approved" {
			sawSelfApproval = true
		}
	}
	if !sawSelfApproval {
		t.Fatal("solo-admin self-approval not recorded in the audit trail")
	}
}

// TestInstanceCostRollup: cost rows across two users aggregate correctly in
// GET /api/admin/costs, and `_instance` spend is included and labeled.
func TestInstanceCostRollup(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	admin := newSession(t, env)
	setRole(t, env, admin.UserID, store.RoleAdmin)
	userA := newSession(t, env)
	userB := newSession(t, env)

	seedCost := func(userID, agent, model string, micros int64) {
		auditID := "audit-" + randSuffix()
		entry := &store.AuditEntry{
			ID: auditID, UserID: userID, RequestID: "req-cost-" + randSuffix(),
			Timestamp: time.Now().UTC(), Service: "llm", Action: "call",
			Decision: "allow", Outcome: "executed",
		}
		if err := env.Store.LogAudit(ctx, entry); err != nil {
			t.Fatalf("LogAudit: %v", err)
		}
		cm := micros
		ag := agent
		if err := env.Store.RecordLLMRequestCost(ctx, &store.LLMRequestCost{
			AuditID: auditID, UserID: userID, AgentID: &ag, RequestID: entry.RequestID,
			Timestamp: time.Now().UTC(), Provider: "anthropic", Model: model,
			InputTokens: 10, OutputTokens: 5, CostMicros: &cm,
		}); err != nil {
			t.Fatalf("RecordLLMRequestCost: %v", err)
		}
	}

	seedCost(userA.UserID, "agent-a", "claude-opus-4-6", 1000)
	seedCost(userA.UserID, "agent-a", "claude-opus-4-6", 500)
	seedCost(userB.UserID, "agent-b", "claude-haiku-4-5", 250)
	seedCost(store.InstanceUserID, "agent-tf", "claude-opus-4-6", 400)

	resp := env.do("GET", "/api/admin/costs?window=daily", admin.AccessToken, nil)
	body := mustStatus(t, resp, http.StatusOK)

	// Total spend across all users, including `_instance`.
	if got := int64(body["cost_micros"].(float64)); got != 2150 {
		t.Fatalf("instance cost_micros = %d, want 2150", got)
	}
	if got := int(body["request_count"].(float64)); got != 4 {
		t.Fatalf("instance request_count = %d, want 4", got)
	}

	byUser := arr(t, body, "by_user")
	if len(byUser) != 3 {
		t.Fatalf("by_user len = %d, want 3 (A, B, _instance)", len(byUser))
	}
	sawInstanceLabel := false
	for _, u := range byUser {
		m := u.(map[string]any)
		if m["user_id"] == store.InstanceUserID {
			if m["owner_label"] != "Terraform / automation" {
				t.Fatalf("_instance cost owner_label = %v, want automation label", m["owner_label"])
			}
			if int64(m["cost_micros"].(float64)) != 400 {
				t.Fatalf("_instance cost = %v, want 400", m["cost_micros"])
			}
			sawInstanceLabel = true
		}
	}
	if !sawInstanceLabel {
		t.Fatal("_instance automation spend missing from rollup (would under-count Terraform/CI usage)")
	}
}

// TestCloudCannotReachOrgBlindListAll (F5): the cloud composition's admin
// routes are absent — an instance admin cannot read another org's
// agents/costs/audit via the org-blind ListAll* endpoints. The same routes
// exist in the OSS build.
func TestCloudCannotReachOrgBlindListAll(t *testing.T) {
	cloud := newCloudCompositionTestEnv(t)
	admin := newSession(t, cloud)
	setRole(t, cloud, admin.UserID, store.RoleAdmin)

	for _, path := range []string{"/api/admin/agents", "/api/admin/approvals", "/api/admin/audit", "/api/admin/costs?window=daily"} {
		resp := cloud.do("GET", path, admin.AccessToken, nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("cloud build %s = %d, want 404 (route must be absent in multi-org build)", path, resp.StatusCode)
		}
	}

	// Sanity: the same routes ARE mounted in the OSS build.
	oss := newTestEnv(t)
	ossAdmin := newSession(t, oss)
	setRole(t, oss, ossAdmin.UserID, store.RoleAdmin)
	resp := oss.do("GET", "/api/admin/agents", ossAdmin.AccessToken, nil)
	mustStatus(t, resp, http.StatusOK)
}

// TestCloudNilOrgIDForAgentStillGatesRoutes (F5 hardening): a cloud build that
// wires WithOrgGov but passes a nil orgIDForAgent must STILL withhold the
// org-blind /api/admin/* routes. Route gating keys off orgGovConfigured, not
// off the optional resolver being non-nil, so a nil resolver can't leak the
// cross-org fleet routes.
func TestCloudNilOrgIDForAgentStillGatesRoutes(t *testing.T) {
	cloud := newCloudCompositionTestEnv(t, nil)
	admin := newSession(t, cloud)
	setRole(t, cloud, admin.UserID, store.RoleAdmin)

	for _, path := range []string{"/api/admin/agents", "/api/admin/approvals", "/api/admin/audit", "/api/admin/costs?window=daily"} {
		resp := cloud.do("GET", path, admin.AccessToken, nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("cloud build (nil orgIDForAgent) %s = %d, want 404 (route must be absent)", path, resp.StatusCode)
		}
	}
}

// TestSoloAdminSelfDenyRecordsSelfDeny (F7): a solo admin denying a hold raised
// by their OWN agent (allow_self_approve=false) is written to the audit trail
// as action=self_deny/outcome=self_denied — NOT the contradictory
// self_approve/self_approved marker the deny path previously hardcoded.
func TestSoloAdminSelfDenyRecordsSelfDeny(t *testing.T) {
	adapter := newMockAdapter("mock.soloselfdeny", "run").withResult("ok", map[string]any{"done": true})
	env := newTestEnvWithConfig(t, config.LLMConfig{}, nil, func(c *config.Config) {
		c.Approval.AllowSelfApprove = false
	}, adapter)

	// Exactly one admin in the instance → the solo-admin self-resolve exception
	// applies.
	admin := newScenario(t, env, "soloadmin")
	setRole(t, env, admin.session.UserID, store.RoleAdmin)

	reqID := raiseHold(t, env, admin, "mock.soloselfdeny")
	id := adminPendingIDForRequest(t, env, admin.session.AccessToken, reqID)

	resp := env.do("POST", "/api/admin/approvals/"+id+"/resolve", admin.session.AccessToken, map[string]any{"decision": "deny"})
	res := mustStatus(t, resp, http.StatusOK)
	if res["decision"] != "deny" {
		t.Fatalf("resolve response = %+v", res)
	}

	resp = env.do("GET", "/api/admin/audit?service=governance", admin.session.AccessToken, nil)
	auditBody := mustStatus(t, resp, http.StatusOK)
	sawSelfDeny := false
	for _, e := range arr(t, auditBody, "entries") {
		m := e.(map[string]any)
		if m["outcome"] == "self_approved" || m["action"] == "self_approve" {
			t.Fatalf("solo-admin self-DENY mislabeled as a self-approval: %+v", m)
		}
		if m["action"] == "self_deny" {
			sawSelfDeny = true
			if m["outcome"] != "self_denied" {
				t.Fatalf("self-deny outcome = %v, want self_denied", m["outcome"])
			}
			if m["decision"] != "deny" {
				t.Fatalf("self-deny decision = %v, want deny", m["decision"])
			}
		}
	}
	if !sawSelfDeny {
		t.Fatal("solo-admin self-deny not recorded as self_deny in the audit trail")
	}
}

// TestAdminDenyPreTaskHoldSharingRequestID: admin-denying a pre-task hold
// (task_id NULL) that shares a request_id with a task-scoped hold resolves the
// exact loaded row (200) instead of re-resolving by (request_id, user_id) and
// spuriously returning 409 (ErrAmbiguous). The task-scoped sibling is left
// untouched.
func TestAdminDenyPreTaskHoldSharingRequestID(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	admin := newSession(t, env)
	setRole(t, env, admin.UserID, store.RoleAdmin)
	member := newSession(t, env)
	setRole(t, env, member.UserID, store.RoleMember)

	reqID := "req-shared-" + randSuffix()
	taskScoped := "task-" + randSuffix()

	preTask := &store.PendingApproval{
		UserID: member.UserID, RequestID: reqID, AuditID: "audit-pre-" + randSuffix(),
		RequestBlob: json.RawMessage(`{"service":"gmail"}`),
		ExpiresAt:   time.Now().Add(time.Hour).UTC(),
	}
	scoped := &store.PendingApproval{
		UserID: member.UserID, RequestID: reqID, TaskID: &taskScoped, AuditID: "audit-task-" + randSuffix(),
		RequestBlob: json.RawMessage(`{"service":"gmail"}`),
		ExpiresAt:   time.Now().Add(time.Hour).UTC(),
	}
	for _, pa := range []*store.PendingApproval{preTask, scoped} {
		if err := env.Store.SavePendingApproval(ctx, pa); err != nil {
			t.Fatalf("SavePendingApproval: %v", err)
		}
	}

	resp := env.do("POST", "/api/admin/approvals/"+preTask.ID+"/resolve", admin.AccessToken, map[string]any{"decision": "deny"})
	res := mustStatus(t, resp, http.StatusOK)
	if res["decision"] != "deny" {
		t.Fatalf("resolve response = %+v", res)
	}

	// The pre-task hold is denied+removed; the task-scoped sibling survives.
	if _, err := env.Store.GetPendingApprovalByID(ctx, preTask.ID); err != store.ErrNotFound {
		t.Fatalf("pre-task hold still present after deny: err=%v", err)
	}
	sibling, err := env.Store.GetPendingApprovalByID(ctx, scoped.ID)
	if err != nil {
		t.Fatalf("task-scoped sibling vanished: %v", err)
	}
	if sibling.Status != "pending" {
		t.Fatalf("task-scoped sibling status = %q, want pending (untouched)", sibling.Status)
	}
}
