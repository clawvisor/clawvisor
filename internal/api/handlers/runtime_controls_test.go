package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/clawvisor/clawvisor/internal/api/middleware"
	"github.com/clawvisor/clawvisor/internal/store/sqlite"
	"github.com/clawvisor/clawvisor/pkg/config"
	"github.com/clawvisor/clawvisor/pkg/store"
)

func TestRuntimeHandlerRuleCRUDAndStarterProfile(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.New(ctx, filepath.Join(t.TempDir(), "runtime-controls.db"))
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	defer db.Close()
	st := sqlite.NewStore(db)
	user, err := st.CreateUser(ctx, "runtime-controls@test.example", "hash")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	agent, err := st.CreateAgent(ctx, user.ID, "runtime-agent", "token-hash")
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	h := NewRuntimeHandler(st, nil, nil, config.Default(), nil)

	createBody := []byte(`{
		"scope":"agent",
		"agent_id":"` + agent.ID + `",
		"kind":"egress",
		"action":"allow",
		"host":"api.example.com",
		"method":"GET",
		"path":"/v1/me",
		"reason":"quiet startup check"
	}`)
	createReq := httptest.NewRequest(http.MethodPost, "/api/runtime/rules", bytes.NewReader(createBody))
	createReq = createReq.WithContext(context.WithValue(createReq.Context(), middleware.UserContextKey, user))
	createRes := httptest.NewRecorder()
	h.CreateRule(createRes, createReq)
	if createRes.Code != http.StatusCreated {
		t.Fatalf("CreateRule status=%d body=%s", createRes.Code, createRes.Body.String())
	}
	var created store.RuntimePolicyRule
	if err := json.Unmarshal(createRes.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode created rule: %v", err)
	}
	if created.AgentID == nil || *created.AgentID != agent.ID {
		t.Fatalf("expected agent-scoped rule, got %+v", created)
	}

	listReq := httptest.NewRequest(http.MethodGet, "/api/runtime/rules?kind=egress&agent_id="+agent.ID, nil)
	listReq = listReq.WithContext(context.WithValue(listReq.Context(), middleware.UserContextKey, user))
	listRes := httptest.NewRecorder()
	h.ListRules(listRes, listReq)
	if listRes.Code != http.StatusOK {
		t.Fatalf("ListRules status=%d body=%s", listRes.Code, listRes.Body.String())
	}
	var listed struct {
		Entries []*store.RuntimePolicyRule `json:"entries"`
		Total   int                        `json:"total"`
	}
	if err := json.Unmarshal(listRes.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode listed rules: %v", err)
	}
	if listed.Total != 1 || len(listed.Entries) != 1 {
		t.Fatalf("expected one created rule, got %+v", listed)
	}

	profileBody := []byte(`{"agent_id":"` + agent.ID + `"}`)
	profileReq := httptest.NewRequest(http.MethodPost, "/api/runtime/starter-profiles/codex/apply", bytes.NewReader(profileBody))
	profileReq.SetPathValue("profile", "codex")
	profileReq = profileReq.WithContext(context.WithValue(profileReq.Context(), middleware.UserContextKey, user))
	profileRes := httptest.NewRecorder()
	h.ApplyStarterProfile(profileRes, profileReq)
	if profileRes.Code != http.StatusOK {
		t.Fatalf("ApplyStarterProfile status=%d body=%s", profileRes.Code, profileRes.Body.String())
	}
	rules, err := st.ListRuntimePolicyRules(ctx, user.ID, store.RuntimePolicyRuleFilter{AgentID: agent.ID, Kind: "egress"})
	if err != nil {
		t.Fatalf("ListRuntimePolicyRules: %v", err)
	}
	if len(rules) < 4 {
		t.Fatalf("expected starter profile rules to be applied, got %d", len(rules))
	}

	decisionBody := []byte(`{"command_key":"codex","profile":"codex","decision":"always_skip"}`)
	decisionReq := httptest.NewRequest(http.MethodPut, "/api/runtime/preset-decisions", bytes.NewReader(decisionBody))
	decisionReq = decisionReq.WithContext(context.WithValue(decisionReq.Context(), middleware.UserContextKey, user))
	decisionRes := httptest.NewRecorder()
	h.UpsertPresetDecision(decisionRes, decisionReq)
	if decisionRes.Code != http.StatusOK {
		t.Fatalf("UpsertPresetDecision status=%d body=%s", decisionRes.Code, decisionRes.Body.String())
	}
	decision, err := st.GetRuntimePresetDecision(ctx, user.ID, "codex", "codex")
	if err != nil {
		t.Fatalf("GetRuntimePresetDecision: %v", err)
	}
	if decision.Decision != "always_skip" {
		t.Fatalf("expected always_skip decision, got %+v", decision)
	}
}

func TestRuntimeHandlerPromoteEventToTask(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.New(ctx, filepath.Join(t.TempDir(), "runtime-event-promote.db"))
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	defer db.Close()
	st := sqlite.NewStore(db)
	user, err := st.CreateUser(ctx, "runtime-promote@test.example", "hash")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	agent, err := st.CreateAgent(ctx, user.ID, "runtime-agent", "token-hash")
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	session := &store.RuntimeSession{
		ID:                    "runtime-session-1",
		UserID:                user.ID,
		AgentID:               agent.ID,
		Mode:                  "proxy",
		ProxyBearerSecretHash: "runtime-secret-hash",
		ObservationMode:       false,
		ExpiresAt:             timeNowUTCForTest().Add(time.Hour),
	}
	if err := st.CreateRuntimeSession(ctx, session); err != nil {
		t.Fatalf("CreateRuntimeSession: %v", err)
	}
	event := &store.RuntimeEvent{
		ID:         "runtime-event-1",
		Timestamp:  timeNowUTCForTest(),
		SessionID:  session.ID,
		UserID:     user.ID,
		AgentID:    agent.ID,
		EventType:  "runtime.egress.review_required",
		ActionKind: "egress",
		Reason:     nullableStr("Allow GitHub profile lookup"),
		MetadataJSON: mustJSON(map[string]any{
			"host":   "api.github.com",
			"method": "GET",
			"path":   "/user",
		}),
	}
	if err := st.CreateRuntimeEvent(ctx, event); err != nil {
		t.Fatalf("CreateRuntimeEvent: %v", err)
	}

	h := NewRuntimeHandler(st, nil, nil, config.Default(), nil)
	body := []byte(`{"lifetime":"session"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/runtime/events/"+event.ID+"/promote-task", bytes.NewReader(body))
	req.SetPathValue("id", event.ID)
	req = req.WithContext(context.WithValue(req.Context(), middleware.UserContextKey, user))
	res := httptest.NewRecorder()
	h.PromoteEventToTask(res, req)
	if res.Code != http.StatusCreated {
		t.Fatalf("PromoteEventToTask status=%d body=%s", res.Code, res.Body.String())
	}
	var payload struct {
		Task store.Task `json:"task"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode promoted task: %v", err)
	}
	if payload.Task.ExpectedEgress == nil || !bytes.Contains(payload.Task.ExpectedEgress, []byte(`"api.github.com"`)) {
		t.Fatalf("expected promoted egress envelope, got %+v", payload.Task)
	}
	binding, err := st.GetActiveTaskSession(ctx, payload.Task.ID, event.SessionID)
	if err != nil {
		t.Fatalf("GetActiveTaskSession: %v", err)
	}
	if binding.TaskID != payload.Task.ID || binding.SessionID != event.SessionID {
		t.Fatalf("expected active task session binding, got %+v", binding)
	}
}

func timeNowUTCForTest() time.Time {
	return time.Date(2026, 4, 29, 12, 0, 0, 0, time.UTC)
}
