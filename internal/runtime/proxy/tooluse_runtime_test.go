package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/leases"
	"github.com/clawvisor/clawvisor/internal/runtime/review"
	"github.com/clawvisor/clawvisor/internal/store/sqlite"
	"github.com/clawvisor/clawvisor/pkg/config"
	"github.com/clawvisor/clawvisor/pkg/store"
)

func TestEnsureHeldToolUseApprovalAndDashboardRelease(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.New(ctx, t.TempDir()+"/tooluse.db")
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := sqlite.NewStore(db)
	userID, agentID := seedRuntimePrincipal(t, st)

	cfg := config.Default()
	cfg.RuntimeProxy.Enabled = true
	cfg.RuntimeProxy.DataDir = t.TempDir()

	task := &store.Task{
		ID:               "task-tool-review",
		UserID:           userID,
		AgentID:          agentID,
		Purpose:          "Review inbox issues",
		Status:           "active",
		Lifetime:         "session",
		SchemaVersion:    2,
		ExpiresInSeconds: 3600,
	}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	session := createRuntimeSession(t, st, "runtime-tool-session", userID, agentID, false)
	runtimeSession, err := st.GetRuntimeSession(ctx, session.id)
	if err != nil {
		t.Fatalf("GetRuntimeSession: %v", err)
	}
	if err := st.UpsertActiveTaskSession(ctx, &store.ActiveTaskSession{
		TaskID:     task.ID,
		SessionID:  session.id,
		UserID:     userID,
		AgentID:    agentID,
		Status:     "active",
		StartedAt:  time.Now().UTC(),
		LastSeenAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("UpsertActiveTaskSession: %v", err)
	}

	srv, err := NewServer(Config{DataDir: t.TempDir(), Addr: "127.0.0.1:0"}, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	hooks := ToolUseHooks{
		Store:       st,
		Config:      cfg,
		ReviewCache: review.NewApprovalCache(),
		Leases:      leases.Service{Store: st},
	}

	rec, held, substitute := srv.ensureHeldToolUseApproval(ctx, hooks, runtimeSession, task, conversationToolUse("toolu_1", "fetch_messages"), map[string]any{"max_results": 10})
	if rec == nil || held == nil {
		t.Fatalf("expected approval record and held approval, got rec=%v held=%v", rec, held)
	}
	if rec.ResolutionTransport != "release_held_tool_use" {
		t.Fatalf("unexpected resolution transport %q", rec.ResolutionTransport)
	}
	if held.ApprovalRecordID != rec.ID || held.TaskID != task.ID {
		t.Fatalf("held approval missing runtime context: %+v", held)
	}
	if substitute == "" {
		t.Fatal("expected substitute prompt")
	}

	if err := st.ResolveApprovalRecord(ctx, rec.ID, "allow_once", "approved", time.Now().UTC()); err != nil {
		t.Fatalf("ResolveApprovalRecord: %v", err)
	}

	reqBody := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"continue"}]}]}`)
	req, _ := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", nil)
	_, resp := srv.syntheticHeldToolUseResponse(req, runtimeSession, hooks, held, true, "approved", reqBody)
	if resp == nil {
		t.Fatal("expected synthetic response")
	}
	defer resp.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode synthetic response: %v", err)
	}
	content := body["content"].([]any)
	block := content[0].(map[string]any)
	if block["type"] != "tool_use" || block["name"] != "fetch_messages" {
		t.Fatalf("unexpected synthetic tool_use block: %+v", block)
	}

	openLeases, err := st.ListOpenToolExecutionLeases(ctx, session.id)
	if err != nil {
		t.Fatalf("ListOpenToolExecutionLeases: %v", err)
	}
	if len(openLeases) != 1 || openLeases[0].ToolUseID != "toolu_1" {
		t.Fatalf("expected one open lease for toolu_1, got %+v", openLeases)
	}

	toolResultBody := []byte(`{
	  "messages":[
	    {"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"fetch_messages","input":{"max_results":10}}]},
	    {"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"ok"}]}
	  ]
	}`)
	toolResultReq, _ := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", nil)
	srv.closeLeasesForToolResults(ctx, hooks, toolResultReq, session.id, toolResultBody)

	openLeases, err = st.ListOpenToolExecutionLeases(ctx, session.id)
	if err != nil {
		t.Fatalf("ListOpenToolExecutionLeases(after close): %v", err)
	}
	if len(openLeases) != 0 {
		t.Fatalf("expected lease to close after tool_result, got %+v", openLeases)
	}
}

func TestParseAnthropicApprovalReply(t *testing.T) {
	t.Parallel()

	verb, id := parseAnthropicApprovalReply([]byte(`{
	  "messages":[
	    {"role":"assistant","content":[{"type":"text","text":"pending approval"}]},
	    {"role":"user","content":[{"type":"text","text":"approve cv-abcdef123456"}]}
	  ]
	}`))
	if verb != "approve" || id != "cv-abcdef123456" {
		t.Fatalf("unexpected explicit approval reply: verb=%q id=%q", verb, id)
	}

	verb, id = parseAnthropicApprovalReply([]byte(`{
	  "messages":[
	    {"role":"user","content":[{"type":"text","text":"deny"}]}
	  ]
	}`))
	if verb != "deny" || id != "" {
		t.Fatalf("unexpected bare approval reply: verb=%q id=%q", verb, id)
	}
}

func TestInlineApprovalResolvesSameApprovalRecord(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.New(ctx, t.TempDir()+"/tooluse-inline.db")
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := sqlite.NewStore(db)
	userID, agentID := seedRuntimePrincipal(t, st)

	task := &store.Task{
		ID:               "task-inline-review",
		UserID:           userID,
		AgentID:          agentID,
		Purpose:          "Review tool calls inline",
		Status:           "active",
		Lifetime:         "session",
		SchemaVersion:    2,
		ExpiresInSeconds: 3600,
	}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	session := createRuntimeSession(t, st, "runtime-inline-session", userID, agentID, false)
	runtimeSession, err := st.GetRuntimeSession(ctx, session.id)
	if err != nil {
		t.Fatalf("GetRuntimeSession: %v", err)
	}

	srv, err := NewServer(Config{DataDir: t.TempDir(), Addr: "127.0.0.1:0"}, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	hooks := ToolUseHooks{
		Store:       st,
		Config:      config.Default(),
		ReviewCache: review.NewApprovalCache(),
		Leases:      leases.Service{Store: st},
	}

	rec, held, _ := srv.ensureHeldToolUseApproval(ctx, hooks, runtimeSession, task, conversationToolUse("toolu_inline", "fetch_messages"), map[string]any{"max_results": 5})
	if rec == nil || held == nil {
		t.Fatalf("expected approval record and held approval, got rec=%v held=%v", rec, held)
	}

	approveBody := fmt.Sprintf(`{
	  "messages":[
	    {"role":"user","content":[{"type":"text","text":"approve %s"}]}
	  ]
	}`, held.ID)
	verb, heldID := parseAnthropicApprovalReply([]byte(approveBody))
	if verb != "approve" || heldID != held.ID {
		t.Fatalf("expected inline approval for held id, got verb=%q id=%q", verb, heldID)
	}

	resolved := hooks.ReviewCache.Resolve(session.id, heldID)
	if resolved == nil {
		t.Fatal("expected held approval to resolve from cache")
	}
	if resolved.ApprovalRecordID != rec.ID {
		t.Fatalf("inline approval should target the canonical approval record, got %q want %q", resolved.ApprovalRecordID, rec.ID)
	}

	if err := st.ResolveApprovalRecord(ctx, resolved.ApprovalRecordID, "allow_once", "approved", time.Now().UTC()); err != nil {
		t.Fatalf("ResolveApprovalRecord: %v", err)
	}
	stored, err := st.GetApprovalRecord(ctx, rec.ID)
	if err != nil {
		t.Fatalf("GetApprovalRecord: %v", err)
	}
	if stored.Status != "approved" || stored.Resolution != "allow_once" {
		t.Fatalf("unexpected approval record after inline approval: %+v", stored)
	}
}

func TestEnsureHeldToolUseApprovalAllowsMultiplePendingApprovalsPerSession(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.New(ctx, t.TempDir()+"/tooluse-multi.db")
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := sqlite.NewStore(db)
	userID, agentID := seedRuntimePrincipal(t, st)

	task := &store.Task{
		ID:               "task-multi-review",
		UserID:           userID,
		AgentID:          agentID,
		Purpose:          "Review multiple tool calls",
		Status:           "active",
		Lifetime:         "session",
		SchemaVersion:    2,
		ExpiresInSeconds: 3600,
	}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	session := createRuntimeSession(t, st, "runtime-multi-session", userID, agentID, false)
	runtimeSession, err := st.GetRuntimeSession(ctx, session.id)
	if err != nil {
		t.Fatalf("GetRuntimeSession: %v", err)
	}

	srv, err := NewServer(Config{DataDir: t.TempDir(), Addr: "127.0.0.1:0"}, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	hooks := ToolUseHooks{
		Store:       st,
		Config:      config.Default(),
		ReviewCache: review.NewApprovalCache(),
		Leases:      leases.Service{Store: st},
	}

	firstRec, firstHeld, _ := srv.ensureHeldToolUseApproval(ctx, hooks, runtimeSession, task, conversationToolUse("toolu_1", "fetch_messages"), map[string]any{"max_results": 10})
	secondRec, secondHeld, _ := srv.ensureHeldToolUseApproval(ctx, hooks, runtimeSession, task, conversationToolUse("toolu_2", "fetch_thread"), map[string]any{"thread_id": "123"})
	if firstRec == nil || secondRec == nil || firstHeld == nil || secondHeld == nil {
		t.Fatalf("expected distinct approval records and held approvals, got %v %v %v %v", firstRec, secondRec, firstHeld, secondHeld)
	}
	if firstRec.ID == secondRec.ID || firstHeld.ID == secondHeld.ID {
		t.Fatal("expected distinct held approvals per blocked tool use")
	}
	if got := hooks.ReviewCache.Count(session.id); got != 2 {
		t.Fatalf("held approval count = %d, want 2", got)
	}
	if got := hooks.ReviewCache.Get(session.id); got == nil || got.ID != firstHeld.ID {
		t.Fatalf("expected first held approval to be released first, got %+v", got)
	}
	if got := hooks.ReviewCache.GetByApprovalRecord(session.id, secondRec.ID); got == nil || got.ID != secondHeld.ID {
		t.Fatalf("expected second held approval lookup, got %+v", got)
	}

	resolvedFirst := hooks.ReviewCache.Resolve(session.id, firstHeld.ID)
	if resolvedFirst == nil || resolvedFirst.ToolUseID != "toolu_1" {
		t.Fatalf("Resolve(first) = %+v", resolvedFirst)
	}
	if got := hooks.ReviewCache.Get(session.id); got == nil || got.ID != secondHeld.ID {
		t.Fatalf("expected second held approval to remain after first resolve, got %+v", got)
	}
}

func TestConsumeDashboardResolvedHeldApprovalSkipsEarlierPendingHold(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.New(ctx, t.TempDir()+"/tooluse-dashboard-order.db")
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := sqlite.NewStore(db)
	userID, agentID := seedRuntimePrincipal(t, st)

	task := &store.Task{
		ID:               "task-dashboard-order",
		UserID:           userID,
		AgentID:          agentID,
		Purpose:          "Release later held approval first",
		Status:           "active",
		Lifetime:         "session",
		SchemaVersion:    2,
		ExpiresInSeconds: 3600,
	}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	session := createRuntimeSession(t, st, "runtime-dashboard-order", userID, agentID, false)
	runtimeSession, err := st.GetRuntimeSession(ctx, session.id)
	if err != nil {
		t.Fatalf("GetRuntimeSession: %v", err)
	}

	srv, err := NewServer(Config{DataDir: t.TempDir(), Addr: "127.0.0.1:0"}, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	hooks := ToolUseHooks{
		Store:       st,
		Config:      config.Default(),
		ReviewCache: review.NewApprovalCache(),
		Leases:      leases.Service{Store: st},
	}

	firstRec, firstHeld, _ := srv.ensureHeldToolUseApproval(ctx, hooks, runtimeSession, task, conversationToolUse("toolu_pending", "fetch_messages"), map[string]any{"max_results": 10})
	secondRec, secondHeld, _ := srv.ensureHeldToolUseApproval(ctx, hooks, runtimeSession, task, conversationToolUse("toolu_ready", "fetch_thread"), map[string]any{"thread_id": "123"})
	if firstRec == nil || secondRec == nil || firstHeld == nil || secondHeld == nil {
		t.Fatalf("expected held approvals, got %v %v %v %v", firstRec, secondRec, firstHeld, secondHeld)
	}
	if err := st.ResolveApprovalRecord(ctx, secondRec.ID, "allow_once", "approved", time.Now().UTC()); err != nil {
		t.Fatalf("ResolveApprovalRecord(second): %v", err)
	}

	resolved, allowed, err := srv.consumeDashboardResolvedHeldApproval(ctx, hooks, session.id)
	if err != nil {
		t.Fatalf("consumeDashboardResolvedHeldApproval: %v", err)
	}
	if !allowed {
		t.Fatal("expected later dashboard-approved held approval to allow")
	}
	if resolved == nil || resolved.ID != secondHeld.ID {
		t.Fatalf("resolved = %+v, want second held approval %+v", resolved, secondHeld)
	}
	if got := hooks.ReviewCache.Count(session.id); got != 1 {
		t.Fatalf("held approval count after resolve = %d, want 1", got)
	}
	if got := hooks.ReviewCache.Get(session.id); got == nil || got.ID != firstHeld.ID {
		t.Fatalf("expected earlier pending held approval to remain, got %+v", got)
	}
}

func TestSyntheticHeldToolUseResponseOpenAIResponsesAndLeaseClose(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.New(ctx, t.TempDir()+"/tooluse-openai.db")
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := sqlite.NewStore(db)
	userID, agentID := seedRuntimePrincipal(t, st)

	task := &store.Task{
		ID:               "task-openai-review",
		UserID:           userID,
		AgentID:          agentID,
		Purpose:          "Review OpenAI tool calls",
		Status:           "active",
		Lifetime:         "session",
		SchemaVersion:    2,
		ExpiresInSeconds: 3600,
	}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	session := createRuntimeSession(t, st, "runtime-openai-session", userID, agentID, false)
	runtimeSession, err := st.GetRuntimeSession(ctx, session.id)
	if err != nil {
		t.Fatalf("GetRuntimeSession: %v", err)
	}

	srv, err := NewServer(Config{DataDir: t.TempDir(), Addr: "127.0.0.1:0"}, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	hooks := ToolUseHooks{
		Store:       st,
		Config:      config.Default(),
		ReviewCache: review.NewApprovalCache(),
		Leases:      leases.Service{Store: st},
	}

	rec, held, _ := srv.ensureHeldToolUseApproval(ctx, hooks, runtimeSession, task, conversationToolUse("call_1", "Bash"), map[string]any{"command": "ls /tmp"})
	if rec == nil || held == nil {
		t.Fatalf("expected approval record and held approval, got rec=%v held=%v", rec, held)
	}
	if err := st.ResolveApprovalRecord(ctx, rec.ID, "allow_once", "approved", time.Now().UTC()); err != nil {
		t.Fatalf("ResolveApprovalRecord: %v", err)
	}

	reqBody := []byte(`{"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"continue"}]}]}`)
	req, _ := http.NewRequest(http.MethodPost, "https://api.openai.com/v1/responses", nil)
	_, resp := srv.syntheticHeldToolUseResponse(req, runtimeSession, hooks, held, true, "approved", reqBody)
	if resp == nil {
		t.Fatal("expected synthetic response")
	}
	defer resp.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode synthetic response: %v", err)
	}
	output := body["output"].([]any)
	block := output[0].(map[string]any)
	if block["type"] != "function_call" || block["name"] != "Bash" || block["call_id"] != "call_1" {
		t.Fatalf("unexpected synthetic function_call block: %+v", block)
	}

	openLeases, err := st.ListOpenToolExecutionLeases(ctx, session.id)
	if err != nil {
		t.Fatalf("ListOpenToolExecutionLeases: %v", err)
	}
	if len(openLeases) != 1 || openLeases[0].ToolUseID != "call_1" {
		t.Fatalf("expected one open lease for call_1, got %+v", openLeases)
	}

	toolResultBody := []byte(`{
	  "input":[
	    {"type":"function_call_output","call_id":"call_1","output":"ok"}
	  ]
	}`)
	toolResultReq, _ := http.NewRequest(http.MethodPost, "https://api.openai.com/v1/responses", nil)
	srv.closeLeasesForToolResults(ctx, hooks, toolResultReq, session.id, toolResultBody)

	openLeases, err = st.ListOpenToolExecutionLeases(ctx, session.id)
	if err != nil {
		t.Fatalf("ListOpenToolExecutionLeases(after close): %v", err)
	}
	if len(openLeases) != 0 {
		t.Fatalf("expected lease to close after function_call_output, got %+v", openLeases)
	}
}

func conversationToolUse(id, name string) conversation.ToolUse {
	input, _ := json.Marshal(map[string]any{"max_results": 10})
	return conversation.ToolUse{
		ID:    id,
		Name:  name,
		Input: input,
	}
}
