package llmproxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/pkg/store"
	"github.com/clawvisor/clawvisor/pkg/store/sqlite"
)

func newBudgetTestStore(t *testing.T) (store.Store, *store.Agent) {
	t.Helper()
	ctx := context.Background()
	db, err := sqlite.New(ctx, filepath.Join(t.TempDir(), "budget.db"))
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := sqlite.NewStore(db)
	user, err := st.CreateUser(ctx, "budget@example.com", "x")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	agent, err := st.CreateAgent(ctx, user.ID, "budget-agent", "agent-token-hash")
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	return st, agent
}

func recordMockCost(ctx context.Context, t *testing.T, st store.Store, agent *store.Agent, taskID string, costMicrosVal int64, tokensVal int, auditID string, requestID string) {
	t.Helper()
	audit := &store.AuditEntry{
		ID:         auditID,
		UserID:     agent.UserID,
		AgentID:    &agent.ID,
		RequestID:  requestID,
		Timestamp:  time.Now(),
		Service:    "anthropic",
		Action:     "lite_proxy.messages.create",
		Decision:   "allow",
		Outcome:    "success",
		ParamsSafe: []byte("{}"),
	}
	if err := st.LogAudit(ctx, audit); err != nil {
		t.Fatalf("LogAudit: %v", err)
	}

	var taskIDPtr *string
	if taskID != "" {
		taskIDPtr = &taskID
	}
	cost := &store.LLMRequestCost{
		AuditID:      auditID,
		UserID:       agent.UserID,
		AgentID:      &agent.ID,
		TaskID:       taskIDPtr,
		RequestID:    requestID,
		Timestamp:    time.Now(),
		Provider:     "anthropic",
		Model:        "claude-opus-4-7",
		InputTokens:  tokensVal / 2,
		OutputTokens: tokensVal / 2,
		CostMicros:   &costMicrosVal,
	}
	if err := st.RecordLLMRequestCost(ctx, cost); err != nil {
		t.Fatalf("RecordLLMRequestCost: %v", err)
	}
}

func TestEvaluateBudget_Rules(t *testing.T) {
	st, agent := newBudgetTestStore(t)
	ctx := context.Background()

	approvals := NewMemoryPendingApprovalCache(10 * time.Minute)
	overrides := NewBudgetOverrideCache()

	// 1. No budget set
	res, err := EvaluateBudget(ctx, st, approvals, overrides, agent, "", conversation.ProviderAnthropic, "conv-1")
	if err != nil {
		t.Fatalf("EvaluateBudget: %v", err)
	}
	if res.Blocked || res.Refused || res.Warning {
		t.Fatalf("expected clean result, got: %+v", res)
	}

	// Setup agent settings with a budget
	settings := &store.AgentRuntimeSettings{
		AgentID:                           agent.ID,
		RuntimeEnabled:                    true,
		RuntimeMode:                       "enforce",
		StarterProfile:                    "none",
		OutboundCredentialMode:            "inherit",
		ConversationAutoApproveThreshold: "none",
	}
	limitCost := int64(1000) // $0.001
	limitTokens := int64(100)
	settings.MaxCostMicros = &limitCost
	settings.MaxTokens = &limitTokens
	if err := st.UpsertAgentRuntimeSettings(ctx, settings); err != nil {
		t.Fatalf("UpsertAgentRuntimeSettings: %v", err)
	}

	// 2. Budget is set, but current spend is 0 (Under Budget)
	res, err = EvaluateBudget(ctx, st, approvals, overrides, agent, "", conversation.ProviderAnthropic, "conv-1")
	if err != nil {
		t.Fatalf("EvaluateBudget: %v", err)
	}
	if res.Blocked || res.Refused || res.Warning {
		t.Fatalf("expected clean result, got: %+v", res)
	}

	// Record cost of $0.0005 (500 micros) and 50 tokens (50% budget) -> Still under warning threshold
	recordMockCost(ctx, t, st, agent, "", 500, 50, "audit-under", "req-under")

	res, err = EvaluateBudget(ctx, st, approvals, overrides, agent, "", conversation.ProviderAnthropic, "conv-1")
	if err != nil {
		t.Fatalf("EvaluateBudget: %v", err)
	}
	if res.Blocked || res.Refused || res.Warning {
		t.Fatalf("expected clean result, got: %+v", res)
	}

	// Record another cost of $0.0004 (400 micros) -> total 900 micros (90% budget) -> Warning triggers!
	recordMockCost(ctx, t, st, agent, "", 400, 40, "audit-warning", "req-warning")

	res, err = EvaluateBudget(ctx, st, approvals, overrides, agent, "", conversation.ProviderAnthropic, "conv-1")
	if err != nil {
		t.Fatalf("EvaluateBudget: %v", err)
	}
	if !res.Blocked || !res.Warning || res.Refused {
		t.Fatalf("expected warning block, got: %+v", res)
	}
	if !strings.Contains(res.Message, "budget is at 90%") {
		t.Errorf("expected budget message, got: %q", res.Message)
	}
	if res.ApprovalID == "" {
		t.Errorf("expected generated ApprovalID")
	}

	// 3. Repeated check should return the same warning with the same ApprovalID without creating a new one
	res2, err := EvaluateBudget(ctx, st, approvals, overrides, agent, "", conversation.ProviderAnthropic, "conv-1")
	if err != nil {
		t.Fatalf("EvaluateBudget repeat: %v", err)
	}
	if res2.ApprovalID != res.ApprovalID {
		t.Errorf("expected same ApprovalID, got: %q vs %q", res2.ApprovalID, res.ApprovalID)
	}

	// 4. Exceeded budget (Cost micros > 1000) -> Hard refuse!
	// Wait, we bypass warning hold checks if cost is fully exceeded
	recordMockCost(ctx, t, st, agent, "", 150, 15, "audit-exceeded", "req-exceeded")
	res, err = EvaluateBudget(ctx, st, approvals, overrides, agent, "", conversation.ProviderAnthropic, "conv-1")
	if err != nil {
		t.Fatalf("EvaluateBudget exceeded: %v", err)
	}
	if !res.Blocked || !res.Refused || res.Warning {
		t.Fatalf("expected hard refusal, got: %+v", res)
	}
}

func TestEvaluateBudget_CachedOverride(t *testing.T) {
	st, agent := newBudgetTestStore(t)
	ctx := context.Background()

	approvals := NewMemoryPendingApprovalCache(10 * time.Minute)
	overrides := NewBudgetOverrideCache()

	settings := &store.AgentRuntimeSettings{
		AgentID:                           agent.ID,
		RuntimeEnabled:                    true,
		RuntimeMode:                       "enforce",
		StarterProfile:                    "none",
		OutboundCredentialMode:            "inherit",
		ConversationAutoApproveThreshold: "none",
	}
	limitCost := int64(100)
	settings.MaxCostMicros = &limitCost
	if err := st.UpsertAgentRuntimeSettings(ctx, settings); err != nil {
		t.Fatalf("UpsertAgentRuntimeSettings: %v", err)
	}

	// Add cost of 95 micros (95% budget)
	recordMockCost(ctx, t, st, agent, "", 95, 10, "audit-ov", "req-ov")

	// Verify it triggers warning
	res, _ := EvaluateBudget(ctx, st, approvals, overrides, agent, "", conversation.ProviderAnthropic, "conv-1")
	if !res.Blocked || !res.Warning {
		t.Fatalf("expected warning block, got: %+v", res)
	}

	// Now add override to overrides cache
	overrides.Add("agent:" + agent.ID)

	// Verify budget check bypasses warning
	res, err := EvaluateBudget(ctx, st, approvals, overrides, agent, "", conversation.ProviderAnthropic, "conv-1")
	if err != nil {
		t.Fatalf("EvaluateBudget: %v", err)
	}
	if res.Blocked || res.Warning {
		t.Fatalf("expected override to allow request, got: %+v", res)
	}

	// 5. Test agent-level override unblocks task-scoped request falling back to agent budget
	expiresAt := time.Now().Add(1 * time.Hour)
	task := &store.Task{
		ID:        "task-123",
		UserID:    agent.UserID,
		AgentID:   agent.ID,
		Status:    "active",
		CreatedAt: time.Now(),
		ExpiresAt: &expiresAt,
	}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	res, err = EvaluateBudget(ctx, st, approvals, overrides, agent, "task-123", conversation.ProviderAnthropic, "conv-1")
	if err != nil {
		t.Fatalf("EvaluateBudget with task fallback: %v", err)
	}
	if res.Blocked || res.Warning {
		t.Fatalf("expected agent override to unblock task fallback request, got: %+v", res)
	}
}

func TestRewriteBudgetApprovalReply_ResolvesHold(t *testing.T) {
	st, agent := newBudgetTestStore(t)
	ctx := context.Background()

	approvals := NewMemoryPendingApprovalCache(10 * time.Minute)
	overrides := NewBudgetOverrideCache()

	settings := &store.AgentRuntimeSettings{
		AgentID:                           agent.ID,
		RuntimeEnabled:                    true,
		RuntimeMode:                       "enforce",
		StarterProfile:                    "none",
		OutboundCredentialMode:            "inherit",
		ConversationAutoApproveThreshold: "none",
	}
	limitCost := int64(100)
	settings.MaxCostMicros = &limitCost
	if err := st.UpsertAgentRuntimeSettings(ctx, settings); err != nil {
		t.Fatalf("UpsertAgentRuntimeSettings: %v", err)
	}

	recordMockCost(ctx, t, st, agent, "", 95, 10, "audit-rw", "req-rw")

	// Trigger warning to create hold
	res, _ := EvaluateBudget(ctx, st, approvals, overrides, agent, "", conversation.ProviderAnthropic, "conv-1")
	holdID := res.ApprovalID

	// Verify hold is in the cache
	peeked, _ := approvals.Peek(ctx, ResolveRequest{
		UserID:         agent.UserID,
		AgentID:        agent.ID,
		Provider:       conversation.ProviderAnthropic,
		ConversationID: "conv-1",
		Stage:          StageBudgetWarning,
	})
	if peeked == nil || peeked.ID != holdID {
		t.Fatalf("expected hold %s in cache", holdID)
	}

	// Mock a user reply "approve" (which usually matches standard approve reply format)
	// Let's create an HTTP request containing the approval reply
	httpReq := httptest.NewRequest("POST", "/api/v1/messages", strings.NewReader(`{
		"messages": [
			{"role": "user", "content": "approve `+holdID+`"}
		]
	}`))
	httpReq.Header.Set("Content-Type", "application/json")

	rewriteReq := TaskReplyRewriteRequest{
		HTTPRequest:     httpReq,
		Provider:        conversation.ProviderAnthropic,
		Body:            []byte(`{"messages": [{"role": "user", "content": "approve ` + holdID + `"}]}`),
		Agent:           agent,
		ConversationID:  "conv-1",
		PendingApproval: approvals,
	}

	rewriteRes, err := RewriteBudgetApprovalReply(ctx, rewriteReq, overrides)
	if err != nil {
		t.Fatalf("RewriteBudgetApprovalReply: %v", err)
	}
	if !rewriteRes.Rewritten {
		t.Fatalf("expected Rewritten=true")
	}

	// Verify override has been added
	if !overrides.Has("agent:" + agent.ID) {
		t.Errorf("expected override to be added")
	}

	// Verify hold was popped from cache
	peekedAfter, _ := approvals.Peek(ctx, ResolveRequest{
		UserID:         agent.UserID,
		AgentID:        agent.ID,
		Provider:       conversation.ProviderAnthropic,
		ConversationID: "conv-1",
		Stage:          StageBudgetWarning,
	})
	if peekedAfter != nil {
		t.Fatalf("expected hold to be deleted after resolve")
	}
}

func TestRateLimitTracker_ThrottleDelay(t *testing.T) {
	tracker := NewRateLimitTracker()

	// Update with safe limits
	header := http.Header{}
	header.Set("anthropic-ratelimit-requests-limit", "100")
	header.Set("anthropic-ratelimit-requests-remaining", "90")
	header.Set("anthropic-ratelimit-requests-reset", "60")
	header.Set("anthropic-ratelimit-tokens-limit", "1000")
	header.Set("anthropic-ratelimit-tokens-remaining", "900")
	header.Set("anthropic-ratelimit-tokens-reset", "60")

	tracker.Update("agent-1", "anthropic", "claude-3-5-sonnet", header)
	delay := tracker.ThrottleDelay("agent-1", "anthropic", "claude-3-5-sonnet")
	if delay != 0 {
		t.Fatalf("expected no delay under normal rate limits, got %v", delay)
	}

	// Update with critical requests remaining (under 10%)
	header.Set("anthropic-ratelimit-requests-remaining", "5")
	tracker.Update("agent-1", "anthropic", "claude-3-5-sonnet", header)
	delay = tracker.ThrottleDelay("agent-1", "anthropic", "claude-3-5-sonnet")
	if delay == 0 {
		t.Fatalf("expected throttling delay, got 0")
	}
	if delay > 5*time.Second {
		t.Errorf("expected delay capped at 5s, got %v", delay)
	}
}

func TestEvaluateBudget_TaskRules(t *testing.T) {
	st, agent := newBudgetTestStore(t)
	ctx := context.Background()

	approvals := NewMemoryPendingApprovalCache(10 * time.Minute)
	overrides := NewBudgetOverrideCache()

	// 1. Create a task with a budget
	expiresAt := time.Now().Add(1 * time.Hour)
	limitCost := int64(1000) // $0.001
	limitTokens := int64(100)
	task := &store.Task{
		ID:            "task-budget",
		UserID:        agent.UserID,
		AgentID:       agent.ID,
		Status:        "active",
		CreatedAt:     time.Now(),
		ExpiresAt:     &expiresAt,
		MaxCostMicros: &limitCost,
		MaxTokens:     &limitTokens,
	}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	// 2. Under budget check (Spend is 0)
	res, err := EvaluateBudget(ctx, st, approvals, overrides, agent, "task-budget", conversation.ProviderAnthropic, "conv-1")
	if err != nil {
		t.Fatalf("EvaluateBudget: %v", err)
	}
	if res.Blocked || res.Refused || res.Warning {
		t.Fatalf("expected clean result, got: %+v", res)
	}

	// 3. Spend is 90% (900 micros) -> Warning triggers!
	recordMockCost(ctx, t, st, agent, "task-budget", 900, 90, "audit-task-warning", "req-task-warning")
	res, err = EvaluateBudget(ctx, st, approvals, overrides, agent, "task-budget", conversation.ProviderAnthropic, "conv-1")
	if err != nil {
		t.Fatalf("EvaluateBudget: %v", err)
	}
	if !res.Blocked || !res.Warning || res.Refused {
		t.Fatalf("expected warning block, got: %+v", res)
	}
	if !strings.Contains(res.Message, "budget is at 90%") {
		t.Errorf("expected budget message, got: %q", res.Message)
	}

	// 4. Spent 1500 micros -> Hard refusal!
	recordMockCost(ctx, t, st, agent, "task-budget", 600, 60, "audit-task-exceeded", "req-task-exceeded")
	res, err = EvaluateBudget(ctx, st, approvals, overrides, agent, "task-budget", conversation.ProviderAnthropic, "conv-1")
	if err != nil {
		t.Fatalf("EvaluateBudget: %v", err)
	}
	if !res.Blocked || !res.Refused || res.Warning {
		t.Fatalf("expected hard refusal, got: %+v", res)
	}
}
