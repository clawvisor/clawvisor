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
	"github.com/clawvisor/clawvisor/pkg/config"
	"github.com/clawvisor/clawvisor/pkg/runtime/toolnames"
	"github.com/clawvisor/clawvisor/pkg/store"
	"github.com/clawvisor/clawvisor/pkg/store/sqlite"
)

func TestRuntimeHandlerRuleCRUDAndStarterProfile(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.New(ctx, filepath.Join(t.TempDir(), "runtime-controls.db"))
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	defer db.Close()
	st := sqlite.NewStore(db)
	user, err := st.CreateUser(ctx, "runtime-controls@test.example", "hash", "")
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
	settings, err := st.GetAgentRuntimeSettings(ctx, agent.ID)
	if err != nil {
		t.Fatalf("GetAgentRuntimeSettings: %v", err)
	}
	if settings.StarterProfile != "codex" {
		t.Fatalf("expected starter profile to persist, got %+v", settings)
	}
	appliedDecision, err := st.GetRuntimePresetDecision(ctx, user.ID, "codex", "codex")
	if err != nil {
		t.Fatalf("GetRuntimePresetDecision(applied): %v", err)
	}
	if appliedDecision.Decision != "applied" {
		t.Fatalf("expected applied decision after starter profile apply, got %+v", appliedDecision)
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

func TestRuntimeHandlerEnablePassthroughRejectsExcessiveTTL(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.New(ctx, filepath.Join(t.TempDir(), "runtime-passthrough-ttl.db"))
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	defer db.Close()
	st := sqlite.NewStore(db)
	user, err := st.CreateUser(ctx, "runtime-passthrough-ttl@test.example", "hash", "")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	cfg := config.Default()
	cfg.ProxyLite.Enabled = true
	h := NewRuntimeHandler(st, nil, nil, cfg, nil)
	body, _ := json.Marshal(map[string]any{
		"ttl_seconds": maxRuntimePassthroughTTLSeconds + 1,
		"reason":      "too long",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/runtime/passthrough", bytes.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), middleware.UserContextKey, user))
	rec := httptest.NewRecorder()
	h.EnablePassthrough(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for excessive ttl, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestRuntimeHandlerEnablePassthroughRequiresConfirmationForGlobalTimed(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.New(ctx, filepath.Join(t.TempDir(), "runtime-passthrough-global-confirm.db"))
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	defer db.Close()
	st := sqlite.NewStore(db)
	user, err := st.CreateUser(ctx, "runtime-passthrough-global-confirm@test.example", "hash", "")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	h := NewRuntimeHandler(st, nil, nil, config.Default(), nil)
	body, _ := json.Marshal(map[string]any{
		"ttl_seconds": 600,
		"reason":      "global break glass",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/runtime/passthrough", bytes.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), middleware.UserContextKey, user))
	rec := httptest.NewRecorder()
	h.EnablePassthrough(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 without global confirmation, got %d body=%s", rec.Code, rec.Body.String())
	}

	body, _ = json.Marshal(map[string]any{
		"ttl_seconds":       600,
		"reason":            "global break glass",
		"confirmation_text": "enable global passthrough",
	})
	req = httptest.NewRequest(http.MethodPost, "/api/runtime/passthrough", bytes.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), middleware.UserContextKey, user))
	rec = httptest.NewRecorder()
	h.EnablePassthrough(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201 with global confirmation, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestRuntimeStatusShowsAgentScopedPassthrough(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.New(ctx, filepath.Join(t.TempDir(), "runtime-passthrough-status.db"))
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	defer db.Close()
	st := sqlite.NewStore(db)
	user, err := st.CreateUser(ctx, "runtime-passthrough-status@test.example", "hash", "")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	agent, err := st.CreateAgent(ctx, user.ID, "runtime-agent", "token-hash")
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	expires := time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano)
	if err := st.CreateRuntimePolicyRule(ctx, &store.RuntimePolicyRule{
		ID:      "agent-scoped-passthrough",
		UserID:  user.ID,
		AgentID: &agent.ID,
		Kind:    runtimePassthroughKind,
		Action:  "allow",
		Path:    expires,
		Reason:  "agent scoped bypass",
		Source:  "break_glass",
		Enabled: true,
	}); err != nil {
		t.Fatalf("CreateRuntimePolicyRule: %v", err)
	}

	cfg := config.Default()
	cfg.ProxyLite.Enabled = true
	h := NewRuntimeHandler(st, nil, nil, cfg, nil)
	req := httptest.NewRequest(http.MethodGet, "/api/runtime/status?agent_id="+agent.ID, nil)
	req = req.WithContext(context.WithValue(req.Context(), middleware.UserContextKey, user))
	res := httptest.NewRecorder()
	h.Status(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("Status status=%d body=%s", res.Code, res.Body.String())
	}
	var payload struct {
		Passthrough runtimePassthroughState `json:"passthrough"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if !payload.Passthrough.Enabled || payload.Passthrough.RuleID != "agent-scoped-passthrough" || payload.Passthrough.AgentID != agent.ID {
		t.Fatalf("status should surface active agent-scoped passthrough for selected agent, got %+v", payload.Passthrough)
	}
}

func TestActivePassthroughPrefersAgentScopedRule(t *testing.T) {
	now := time.Now().UTC()
	agentID := "agent-1"
	globalExpiry := now.Add(10 * time.Minute).Format(time.RFC3339Nano)
	agentExpiry := now.Add(5 * time.Minute).Format(time.RFC3339Nano)
	state := activePassthroughFromRules([]*store.RuntimePolicyRule{
		{
			ID:        "global",
			Kind:      runtimePassthroughKind,
			Action:    "allow",
			Path:      globalExpiry,
			Enabled:   true,
			CreatedAt: now.Add(2 * time.Minute),
		},
		{
			ID:        "agent",
			AgentID:   &agentID,
			Kind:      runtimePassthroughKind,
			Action:    "allow",
			Path:      agentExpiry,
			Enabled:   true,
			CreatedAt: now,
		},
	}, agentID, now)
	if !state.Enabled || state.RuleID != "agent" || state.AgentID != agentID {
		t.Fatalf("expected agent-scoped passthrough to win, got %+v", state)
	}
}

func TestRuntimeHandlerToolControlsDiscoverAndUpsert(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.New(ctx, filepath.Join(t.TempDir(), "runtime-tool-controls.db"))
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	defer db.Close()
	st := sqlite.NewStore(db)
	user, err := st.CreateUser(ctx, "runtime-tool-controls@test.example", "hash", "")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	agent, err := st.CreateAgent(ctx, user.ID, "claude-code", "token-hash")
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	if err := st.LogAudit(ctx, &store.AuditEntry{
		ID:        "audit-tools-request",
		UserID:    user.ID,
		AgentID:   &agent.ID,
		RequestID: "req-1",
		Timestamp: time.Now().UTC(),
		Service:   "anthropic",
		Action:    "lite_proxy.messages.create",
		ParamsSafe: json.RawMessage(`{
			"event":"lite_proxy.endpoint_call",
			"available_tools":["Bash","Read"]
		}`),
		Decision: "allow",
		Outcome:  "success",
	}); err != nil {
		t.Fatalf("LogAudit(endpoint): %v", err)
	}
	if err := st.LogAudit(ctx, &store.AuditEntry{
		ID:        "audit-tool-use",
		UserID:    user.ID,
		AgentID:   &agent.ID,
		RequestID: "req-2",
		Timestamp: time.Now().UTC().Add(time.Second),
		Service:   "runtime.tool_use",
		Action:    "lite_proxy.tool_use.allow",
		ParamsSafe: json.RawMessage(`{
			"event":"lite_proxy.tool_use_inspected",
			"tool_name":"Write"
		}`),
		Decision: "allow",
		Outcome:  "task_scope_missing",
	}); err != nil {
		t.Fatalf("LogAudit(tool): %v", err)
	}

	h := NewRuntimeHandler(st, nil, nil, config.Default(), nil)
	listReq := httptest.NewRequest(http.MethodGet, "/api/runtime/tool-controls?agent_id="+agent.ID, nil)
	listReq = listReq.WithContext(context.WithValue(listReq.Context(), middleware.UserContextKey, user))
	listRes := httptest.NewRecorder()
	h.ListToolControls(listRes, listReq)
	if listRes.Code != http.StatusOK {
		t.Fatalf("ListToolControls status=%d body=%s", listRes.Code, listRes.Body.String())
	}
	var listed struct {
		Entries []runtimeToolControlResponse `json:"entries"`
		Total   int                          `json:"total"`
	}
	if err := json.Unmarshal(listRes.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode listed controls: %v", err)
	}
	if listed.Total != 3 {
		t.Fatalf("expected 3 discovered tools, got %+v", listed)
	}
	seenReadDefault := false
	seenBashReadOnlyDefault := false
	for _, entry := range listed.Entries {
		if entry.ToolName == "Read" && entry.Action == "allow" && entry.Scope == "agent" {
			seenReadDefault = true
		}
		if entry.ToolName == "Bash" && entry.ReadOnlyCommandsAllowed {
			seenBashReadOnlyDefault = true
		}
	}
	if !seenReadDefault {
		t.Fatalf("discovered default tools should be seeded as agent-scoped always-allow policies, got %+v", listed.Entries)
	}
	if !seenBashReadOnlyDefault {
		t.Fatalf("shell-like tools should default to allowing read-only commands, got %+v", listed.Entries)
	}

	upsertReq := httptest.NewRequest(http.MethodPut, "/api/runtime/tool-controls", bytes.NewReader([]byte(`{
		"agent_id":"`+agent.ID+`",
		"tool_name":"Bash",
		"action":"review"
	}`)))
	upsertReq = upsertReq.WithContext(context.WithValue(upsertReq.Context(), middleware.UserContextKey, user))
	upsertRes := httptest.NewRecorder()
	h.UpsertToolControl(upsertRes, upsertReq)
	if upsertRes.Code != http.StatusOK {
		t.Fatalf("UpsertToolControl status=%d body=%s", upsertRes.Code, upsertRes.Body.String())
	}
	rules, err := st.ListRuntimePolicyRules(ctx, user.ID, store.RuntimePolicyRuleFilter{AgentID: agent.ID, Kind: "tool"})
	if err != nil {
		t.Fatalf("ListRuntimePolicyRules: %v", err)
	}
	bashRuleCount := func(action string) int {
		count := 0
		for _, rule := range rules {
			if rule.ToolName == "Bash" && rule.Action == action && rule.AgentID != nil && *rule.AgentID == agent.ID {
				count++
			}
		}
		return count
	}
	if bashRuleCount("review") != 1 {
		t.Fatalf("expected one Bash review rule, got %+v", rules)
	}

	allowReq := httptest.NewRequest(http.MethodPut, "/api/runtime/tool-controls", bytes.NewReader([]byte(`{
		"agent_id":"`+agent.ID+`",
		"tool_name":"Bash",
		"action":"allow"
	}`)))
	allowReq = allowReq.WithContext(context.WithValue(allowReq.Context(), middleware.UserContextKey, user))
	allowRes := httptest.NewRecorder()
	h.UpsertToolControl(allowRes, allowReq)
	if allowRes.Code != http.StatusOK {
		t.Fatalf("UpsertToolControl allow status=%d body=%s", allowRes.Code, allowRes.Body.String())
	}
	rules, err = st.ListRuntimePolicyRules(ctx, user.ID, store.RuntimePolicyRuleFilter{AgentID: agent.ID, Kind: "tool"})
	if err != nil {
		t.Fatalf("ListRuntimePolicyRules after allow: %v", err)
	}
	if bashRuleCount("allow") != 1 {
		t.Fatalf("allow should persist an always-allow tool-control rule, got %+v", rules)
	}

	unsetReq := httptest.NewRequest(http.MethodPut, "/api/runtime/tool-controls", bytes.NewReader([]byte(`{
		"agent_id":"`+agent.ID+`",
		"tool_name":"Bash",
		"action":"unset"
	}`)))
	unsetReq = unsetReq.WithContext(context.WithValue(unsetReq.Context(), middleware.UserContextKey, user))
	unsetRes := httptest.NewRecorder()
	h.UpsertToolControl(unsetRes, unsetReq)
	if unsetRes.Code != http.StatusOK {
		t.Fatalf("UpsertToolControl unset status=%d body=%s", unsetRes.Code, unsetRes.Body.String())
	}
	rules, err = st.ListRuntimePolicyRules(ctx, user.ID, store.RuntimePolicyRuleFilter{AgentID: agent.ID, Kind: "tool"})
	if err != nil {
		t.Fatalf("ListRuntimePolicyRules after unset: %v", err)
	}
	if bashRuleCount("review") != 1 {
		t.Fatalf("unset should leave an agent-scoped task-scope fallback marker, got %+v", rules)
	}

	disableReadOnlyReq := httptest.NewRequest(http.MethodPut, "/api/runtime/tool-controls", bytes.NewReader([]byte(`{
		"agent_id":"`+agent.ID+`",
		"tool_name":"Bash",
		"read_only_commands_allowed":false
	}`)))
	disableReadOnlyReq = disableReadOnlyReq.WithContext(context.WithValue(disableReadOnlyReq.Context(), middleware.UserContextKey, user))
	disableReadOnlyRes := httptest.NewRecorder()
	h.UpsertToolControl(disableReadOnlyRes, disableReadOnlyReq)
	if disableReadOnlyRes.Code != http.StatusOK {
		t.Fatalf("UpsertToolControl read-only toggle status=%d body=%s", disableReadOnlyRes.Code, disableReadOnlyRes.Body.String())
	}
	rules, err = st.ListRuntimePolicyRules(ctx, user.ID, store.RuntimePolicyRuleFilter{AgentID: agent.ID, Kind: "tool"})
	if err != nil {
		t.Fatalf("ListRuntimePolicyRules after read-only toggle: %v", err)
	}
	foundReadOnlyDeny := false
	for _, rule := range rules {
		if rule.ToolName == "Bash" && rule.Source == toolnames.ReadOnlyShellSettingSource && rule.Action == "deny" {
			foundReadOnlyDeny = true
		}
	}
	if !foundReadOnlyDeny {
		t.Fatalf("expected read-only shell deny setting, got %+v", rules)
	}

	disableGuardReq := httptest.NewRequest(http.MethodPut, "/api/runtime/tool-controls", bytes.NewReader([]byte(`{
		"agent_id":"`+agent.ID+`",
		"tool_name":"Bash",
		"sensitive_file_guard_enabled":false
	}`)))
	disableGuardReq = disableGuardReq.WithContext(context.WithValue(disableGuardReq.Context(), middleware.UserContextKey, user))
	disableGuardRes := httptest.NewRecorder()
	h.UpsertToolControl(disableGuardRes, disableGuardReq)
	if disableGuardRes.Code != http.StatusOK {
		t.Fatalf("UpsertToolControl guard toggle status=%d body=%s", disableGuardRes.Code, disableGuardRes.Body.String())
	}
	rules, err = st.ListRuntimePolicyRules(ctx, user.ID, store.RuntimePolicyRuleFilter{AgentID: agent.ID, Kind: "tool"})
	if err != nil {
		t.Fatalf("ListRuntimePolicyRules after guard toggle: %v", err)
	}
	foundGuardAllow := false
	for _, rule := range rules {
		if rule.ToolName == "Bash" && rule.Source == toolnames.SensitiveFileGuardSettingSource && rule.Action == "allow" {
			foundGuardAllow = true
		}
	}
	if !foundGuardAllow {
		t.Fatalf("expected sensitive-file-guard allow marker (=disabled), got %+v", rules)
	}
	if toolnames.SensitiveFileGuardEnabled("Bash", agent.ID, rules) {
		t.Fatal("expected guard to be DISABLED after agent-scoped allow marker")
	}

	enableGuardReq := httptest.NewRequest(http.MethodPut, "/api/runtime/tool-controls", bytes.NewReader([]byte(`{
		"agent_id":"`+agent.ID+`",
		"tool_name":"Bash",
		"sensitive_file_guard_enabled":true
	}`)))
	enableGuardReq = enableGuardReq.WithContext(context.WithValue(enableGuardReq.Context(), middleware.UserContextKey, user))
	enableGuardRes := httptest.NewRecorder()
	h.UpsertToolControl(enableGuardRes, enableGuardReq)
	if enableGuardRes.Code != http.StatusOK {
		t.Fatalf("UpsertToolControl re-enable guard status=%d body=%s", enableGuardRes.Code, enableGuardRes.Body.String())
	}
	rules, err = st.ListRuntimePolicyRules(ctx, user.ID, store.RuntimePolicyRuleFilter{AgentID: agent.ID, Kind: "tool"})
	if err != nil {
		t.Fatalf("ListRuntimePolicyRules after re-enable: %v", err)
	}
	if !toolnames.SensitiveFileGuardEnabled("Bash", agent.ID, rules) {
		t.Fatal("expected guard ENABLED after re-enable toggle")
	}
}

func TestRuntimeHandlerToolControlsPreferCurrentShellToolForReadOnlySetting(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.New(ctx, filepath.Join(t.TempDir(), "runtime-tool-controls-shell-alias.db"))
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	defer db.Close()
	st := sqlite.NewStore(db)
	user, err := st.CreateUser(ctx, "runtime-tool-controls-shell-alias@test.example", "hash", "")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	agent, err := st.CreateAgent(ctx, user.ID, "codex-3", "token-hash")
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	now := time.Now().UTC()
	for _, entry := range []*store.AuditEntry{
		{
			ID:        "audit-old-claude-tools",
			UserID:    user.ID,
			AgentID:   &agent.ID,
			RequestID: "req-old",
			Timestamp: now,
			Service:   "anthropic",
			Action:    "lite_proxy.messages.create",
			ParamsSafe: json.RawMessage(`{
				"event":"lite_proxy.endpoint_call",
				"available_tools":["Bash","Read"]
			}`),
			Decision: "allow",
			Outcome:  "success",
		},
		{
			ID:        "audit-current-codex-tools",
			UserID:    user.ID,
			AgentID:   &agent.ID,
			RequestID: "req-current",
			Timestamp: now.Add(time.Second),
			Service:   "openai",
			Action:    "lite_proxy.responses.create",
			ParamsSafe: json.RawMessage(`{
				"event":"lite_proxy.endpoint_call",
				"available_tools":["exec_command","read_file"]
			}`),
			Decision: "allow",
			Outcome:  "success",
		},
		{
			ID:        "audit-stale-bash-tool-use",
			UserID:    user.ID,
			AgentID:   &agent.ID,
			RequestID: "req-stale-tool",
			Timestamp: now.Add(2 * time.Second),
			Service:   "runtime.tool_use",
			Action:    "lite_proxy.tool_use.allow",
			ParamsSafe: json.RawMessage(`{
				"event":"lite_proxy.tool_use_inspected",
				"tool_name":"Bash"
			}`),
			Decision: "allow",
			Outcome:  "success",
		},
	} {
		if err := st.LogAudit(ctx, entry); err != nil {
			t.Fatalf("LogAudit(%s): %v", entry.ID, err)
		}
	}
	if err := st.CreateRuntimePolicyRule(ctx, &store.RuntimePolicyRule{
		ID:         "global-bash-simple",
		UserID:     user.ID,
		Kind:       "tool",
		Action:     "deny",
		ToolName:   "Bash",
		InputShape: json.RawMessage(`{}`),
		Reason:     "old shell policy",
		Source:     "user",
		Enabled:    true,
	}); err != nil {
		t.Fatalf("CreateRuntimePolicyRule simple: %v", err)
	}
	if err := st.CreateRuntimePolicyRule(ctx, &store.RuntimePolicyRule{
		ID:         "global-readonly-bash-marker",
		UserID:     user.ID,
		Kind:       "tool",
		Action:     "deny",
		ToolName:   "Bash",
		InputShape: toolnames.ReadOnlyShellSettingInputShape(),
		Reason:     "Require task scope or approval for read-only shell commands",
		Source:     toolnames.ReadOnlyShellSettingSource,
		Enabled:    true,
	}); err != nil {
		t.Fatalf("CreateRuntimePolicyRule: %v", err)
	}

	h := NewRuntimeHandler(st, nil, nil, config.Default(), nil)
	req := httptest.NewRequest(http.MethodGet, "/api/runtime/tool-controls?agent_id="+agent.ID, nil)
	req = req.WithContext(context.WithValue(req.Context(), middleware.UserContextKey, user))
	res := httptest.NewRecorder()
	h.ListToolControls(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("ListToolControls status=%d body=%s", res.Code, res.Body.String())
	}
	var listed struct {
		Entries []runtimeToolControlResponse `json:"entries"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode listed controls: %v", err)
	}
	byTool := map[string]runtimeToolControlResponse{}
	for _, entry := range listed.Entries {
		byTool[entry.ToolName] = entry
	}
	if _, ok := byTool["Bash"]; ok {
		t.Fatalf("stale Bash shell alias should not be surfaced for a Codex tool snapshot, got %+v", listed.Entries)
	}
	execEntry, ok := byTool["exec_command"]
	if !ok {
		t.Fatalf("expected exec_command control, got %+v", listed.Entries)
	}
	if execEntry.GlobalAction != "deny" || execEntry.GlobalRuleID != "global-bash-simple" {
		t.Fatalf("shell-class simple policy should render on exec_command, got %+v", execEntry)
	}
	if execEntry.GlobalReadOnlyCommandsAllowed == nil || *execEntry.GlobalReadOnlyCommandsAllowed {
		t.Fatalf("read-only shell setting should attach to exec_command, got %+v", execEntry)
	}
	if execEntry.ReadOnlyCommandsAllowed {
		t.Fatalf("effective read-only setting should be disabled on exec_command, got %+v", execEntry)
	}
}

func TestDefaultToolRulesSeedAndRespectUnset(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.New(ctx, filepath.Join(t.TempDir(), "runtime-default-readonly-tools.db"))
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	defer db.Close()
	st := sqlite.NewStore(db)
	user, err := st.CreateUser(ctx, "runtime-default-readonly@test.example", "hash", "")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	agent, err := st.CreateAgent(ctx, user.ID, "codex", "token-hash")
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}

	h := NewLLMEndpointHandler(st, nil, nil)
	h.ensureDefaultToolRules(ctx, agent, []string{
		"Read",
		"read_file",
		"view_image",
		"update_plan",
		"request_user_input",
		"tool_suggest",
		"wait_agent",
		"close_agent",
		"resume_agent",
		"list_mcp_resources",
		"list_mcp_resource_templates",
		"read_mcp_resource",
		"Bash",
		"Write",
		"sessions_list",
		"sessions_history",
		"session_status",
		"sessions_yield",
		"memory_search",
		"memory_get",
		"exec",
		"process",
		"web_fetch",
		"sessions_send",
		"sessions_spawn",
		"subagents",
		"search_files",
		"session_search",
		"skills_list",
		"skill_view",
		"clarify",
		"browser_snapshot",
		"browser_console",
		"browser_get_images",
		"terminal",
		"write_file",
		"patch",
		"browser_navigate",
		"browser_click",
		"browser_type",
		"browser_vision",
		"delegate_task",
		"exec_command",
		"write_stdin",
		"apply_patch",
		"spawn_agent",
		"send_input",
	})
	enabled := true
	rules, err := st.ListRuntimePolicyRules(ctx, user.ID, store.RuntimePolicyRuleFilter{AgentID: agent.ID, Kind: "tool", Enabled: &enabled})
	if err != nil {
		t.Fatalf("ListRuntimePolicyRules: %v", err)
	}
	allowed := map[string]bool{}
	for _, rule := range rules {
		allowed[rule.ToolName] = rule.Action == "allow"
		if (rule.ToolName == "Read" || rule.ToolName == "read_file") && (rule.AgentID == nil || *rule.AgentID != agent.ID) {
			t.Fatalf("default tool rule should be agent-scoped, got %+v", rule)
		}
	}
	if !allowed["Read"] || !allowed["read_file"] || !allowed["view_image"] ||
		!allowed["update_plan"] || !allowed["request_user_input"] ||
		!allowed["tool_suggest"] || !allowed["wait_agent"] ||
		!allowed["close_agent"] || !allowed["resume_agent"] ||
		!allowed["list_mcp_resources"] || !allowed["list_mcp_resource_templates"] ||
		!allowed["read_mcp_resource"] ||
		!allowed["sessions_list"] || !allowed["sessions_history"] ||
		!allowed["session_status"] || !allowed["sessions_yield"] ||
		!allowed["memory_search"] || !allowed["memory_get"] ||
		!allowed["search_files"] || !allowed["session_search"] ||
		!allowed["skills_list"] || !allowed["skill_view"] ||
		!allowed["clarify"] || !allowed["browser_snapshot"] ||
		!allowed["browser_console"] || !allowed["browser_get_images"] {
		t.Fatalf("expected discovered default tools to be seeded as always allow, got %+v", rules)
	}
	if allowed["Bash"] || allowed["Write"] || allowed["exec"] || allowed["process"] ||
		allowed["web_fetch"] || allowed["sessions_send"] || allowed["sessions_spawn"] ||
		allowed["subagents"] || allowed["terminal"] || allowed["write_file"] ||
		allowed["patch"] || allowed["browser_navigate"] || allowed["browser_click"] ||
		allowed["browser_type"] || allowed["browser_vision"] || allowed["delegate_task"] ||
		allowed["exec_command"] || allowed["write_stdin"] || allowed["apply_patch"] ||
		allowed["spawn_agent"] || allowed["send_input"] {
		t.Fatalf("non-default tools should not be seeded as always allow, got %+v", rules)
	}

	unsetReq := httptest.NewRequest(http.MethodPut, "/api/runtime/tool-controls", bytes.NewReader([]byte(`{
		"agent_id":"`+agent.ID+`",
		"tool_name":"Read",
		"action":"unset"
	}`)))
	unsetReq = unsetReq.WithContext(context.WithValue(unsetReq.Context(), middleware.UserContextKey, user))
	unsetRes := httptest.NewRecorder()
	NewRuntimeHandler(st, nil, nil, config.Default(), nil).UpsertToolControl(unsetRes, unsetReq)
	if unsetRes.Code != http.StatusOK {
		t.Fatalf("UpsertToolControl unset status=%d body=%s", unsetRes.Code, unsetRes.Body.String())
	}
	h.ensureDefaultToolRules(ctx, agent, []string{"Read"})
	rules, err = st.ListRuntimePolicyRules(ctx, user.ID, store.RuntimePolicyRuleFilter{AgentID: agent.ID, Kind: "tool", Enabled: &enabled})
	if err != nil {
		t.Fatalf("ListRuntimePolicyRules after unset: %v", err)
	}
	agentReadAllow := 0
	agentReadReview := 0
	for _, rule := range rules {
		if rule.ToolName != "Read" {
			continue
		}
		if rule.AgentID != nil && *rule.AgentID == agent.ID && rule.Action == "allow" {
			agentReadAllow++
		}
		if rule.AgentID != nil && *rule.AgentID == agent.ID && rule.Action == "review" {
			agentReadReview++
		}
	}
	if agentReadAllow != 0 || agentReadReview != 1 {
		t.Fatalf("unset should replace the agent default with one agent task-scope marker, got %+v", rules)
	}
}

func TestDefaultToolRulesMigratesStaleGlobalSystemDefaultsToAgent(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.New(ctx, filepath.Join(t.TempDir(), "runtime-default-readonly-migrate.db"))
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	defer db.Close()
	st := sqlite.NewStore(db)
	user, err := st.CreateUser(ctx, "runtime-default-readonly-migrate@test.example", "hash", "")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	agent, err := st.CreateAgent(ctx, user.ID, "dev-test", "token-hash")
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	if err := st.CreateRuntimePolicyRule(ctx, &store.RuntimePolicyRule{
		ID:         "stale-global-read",
		UserID:     user.ID,
		Kind:       "tool",
		Action:     "allow",
		ToolName:   "Read",
		InputShape: json.RawMessage(`{}`),
		Reason:     "old global default allow",
		Source:     "system",
		Enabled:    true,
	}); err != nil {
		t.Fatalf("CreateRuntimePolicyRule: %v", err)
	}

	NewLLMEndpointHandler(st, nil, nil).ensureDefaultToolRules(ctx, agent, []string{"Read"})
	rules, err := st.ListRuntimePolicyRules(ctx, user.ID, store.RuntimePolicyRuleFilter{AgentID: agent.ID, Kind: "tool"})
	if err != nil {
		t.Fatalf("ListRuntimePolicyRules: %v", err)
	}
	if len(rules) != 2 {
		t.Fatalf("expected stale global rule to remain as backstop plus one agent-scoped rule, got %+v", rules)
	}
	agentRead := 0
	for _, rule := range rules {
		if rule.ToolName == "Read" && rule.AgentID != nil && *rule.AgentID == agent.ID && rule.Action == "allow" {
			agentRead++
		}
	}
	if agentRead != 1 {
		t.Fatalf("expected stale global default to seed an agent allow, got %+v", rules)
	}
}

func TestToolControlsListPreservesAgentScopedDefaultToolRules(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.New(ctx, filepath.Join(t.TempDir(), "runtime-tool-controls-migrate-default.db"))
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	defer db.Close()
	st := sqlite.NewStore(db)
	user, err := st.CreateUser(ctx, "runtime-tool-controls-migrate@test.example", "hash", "")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	agent, err := st.CreateAgent(ctx, user.ID, "dev-test", "token-hash")
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	agentID := agent.ID
	if err := st.CreateRuntimePolicyRule(ctx, &store.RuntimePolicyRule{
		ID:         "stale-agent-read-list",
		UserID:     user.ID,
		AgentID:    &agentID,
		Kind:       "tool",
		Action:     "allow",
		ToolName:   "Read",
		InputShape: json.RawMessage(`{}`),
		Reason:     "old default allow",
		Source:     "system",
		Enabled:    true,
	}); err != nil {
		t.Fatalf("CreateRuntimePolicyRule: %v", err)
	}

	h := NewRuntimeHandler(st, nil, nil, config.Default(), nil)
	req := httptest.NewRequest(http.MethodGet, "/api/runtime/tool-controls?agent_id="+agent.ID, nil)
	req = req.WithContext(context.WithValue(req.Context(), middleware.UserContextKey, user))
	res := httptest.NewRecorder()
	h.ListToolControls(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("ListToolControls status=%d body=%s", res.Code, res.Body.String())
	}
	var listed struct {
		Entries []runtimeToolControlResponse `json:"entries"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode listed controls: %v", err)
	}
	if len(listed.Entries) != 1 || listed.Entries[0].ToolName != "Read" || listed.Entries[0].Scope != "agent" {
		t.Fatalf("agent default should render as an agent policy, got %+v", listed)
	}
}

func TestToolControlsListSeedsClaudeDefaultToolsFromDiscovery(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.New(ctx, filepath.Join(t.TempDir(), "runtime-tool-controls-claude-readonly.db"))
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	defer db.Close()
	st := sqlite.NewStore(db)
	user, err := st.CreateUser(ctx, "runtime-tool-controls-claude-readonly@test.example", "hash", "")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	agent, err := st.CreateAgent(ctx, user.ID, "test-3", "token-hash")
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	if err := st.LogAudit(ctx, &store.AuditEntry{
		ID:        "audit-claude-tools-request",
		UserID:    user.ID,
		AgentID:   &agent.ID,
		RequestID: "req-claude-tools",
		Timestamp: time.Now().UTC(),
		Service:   "anthropic",
		Action:    "lite_proxy.messages.create",
		ParamsSafe: json.RawMessage(`{
			"event":"lite_proxy.endpoint_call",
			"available_tools":[
				"Agent","AskUserQuestion","Bash","CronCreate","CronDelete","CronList",
				"Edit","EnterPlanMode","EnterWorktree","ExitPlanMode","ExitWorktree",
				"LSP","Monitor","NotebookEdit","PushNotification","Read","RemoteTrigger",
				"ScheduleWakeup","Skill","TaskCreate","TaskGet","TaskList","TaskOutput",
				"TaskStop","TaskUpdate","WebFetch","WebSearch","Write"
			]
		}`),
		Decision: "allow",
		Outcome:  "success",
	}); err != nil {
		t.Fatalf("LogAudit(endpoint): %v", err)
	}

	h := NewRuntimeHandler(st, nil, nil, config.Default(), nil)
	req := httptest.NewRequest(http.MethodGet, "/api/runtime/tool-controls?agent_id="+agent.ID, nil)
	req = req.WithContext(context.WithValue(req.Context(), middleware.UserContextKey, user))
	res := httptest.NewRecorder()
	h.ListToolControls(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("ListToolControls status=%d body=%s", res.Code, res.Body.String())
	}
	var listed struct {
		Entries []runtimeToolControlResponse `json:"entries"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode listed controls: %v", err)
	}
	byTool := map[string]runtimeToolControlResponse{}
	for _, entry := range listed.Entries {
		byTool[entry.ToolName] = entry
	}
	for _, toolName := range []string{
		"Agent", "AskUserQuestion", "CronCreate", "CronDelete", "CronList",
		"EnterPlanMode", "EnterWorktree", "ExitPlanMode", "ExitWorktree",
		"LSP", "Monitor", "PushNotification", "Read", "RemoteTrigger",
		"ScheduleWakeup", "Skill", "TaskCreate", "TaskGet", "TaskList",
		"TaskOutput", "TaskStop", "TaskUpdate",
	} {
		entry, ok := byTool[toolName]
		if !ok {
			t.Fatalf("expected discovered tool %q in response, got %+v", toolName, listed.Entries)
		}
		if entry.Action != "allow" || entry.Scope != "agent" {
			t.Fatalf("expected %s to be seeded as agent-scoped always-allow, got %+v", toolName, entry)
		}
	}
	for _, toolName := range []string{"Bash", "Edit", "NotebookEdit", "WebFetch", "WebSearch", "Write"} {
		entry, ok := byTool[toolName]
		if !ok {
			t.Fatalf("expected discovered tool %q in response, got %+v", toolName, listed.Entries)
		}
		if entry.Action != "unset" || entry.Scope != "unset" {
			t.Fatalf("expected %s to remain unset, got %+v", toolName, entry)
		}
	}
}

func TestRuntimeHandlerToolControlsScopesGlobalAdvancedRules(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.New(ctx, filepath.Join(t.TempDir(), "runtime-tool-controls-global-advanced.db"))
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	defer db.Close()
	st := sqlite.NewStore(db)
	user, err := st.CreateUser(ctx, "runtime-tool-controls-global@test.example", "hash", "")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	agent, err := st.CreateAgent(ctx, user.ID, "codex", "token-hash")
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	if err := st.CreateRuntimePolicyRule(ctx, &store.RuntimePolicyRule{
		ID:         "global-bash-advanced",
		UserID:     user.ID,
		Kind:       "tool",
		Action:     "deny",
		ToolName:   "Bash",
		InputRegex: "rm -rf",
		Reason:     "global Bash guard",
		Source:     "user",
		Enabled:    true,
	}); err != nil {
		t.Fatalf("CreateRuntimePolicyRule: %v", err)
	}

	h := NewRuntimeHandler(st, nil, nil, config.Default(), nil)
	req := httptest.NewRequest(http.MethodGet, "/api/runtime/tool-controls?agent_id="+agent.ID, nil)
	req = req.WithContext(context.WithValue(req.Context(), middleware.UserContextKey, user))
	res := httptest.NewRecorder()
	h.ListToolControls(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("ListToolControls status=%d body=%s", res.Code, res.Body.String())
	}
	var listed struct {
		Entries []runtimeToolControlResponse `json:"entries"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode listed controls: %v", err)
	}
	if len(listed.Entries) != 1 {
		t.Fatalf("expected one Bash control, got %+v", listed)
	}
	got := listed.Entries[0]
	if got.ToolName != "Bash" || got.Scope != "global" || got.AdvancedRuleCount != 1 {
		t.Fatalf("global advanced rule should be surfaced as global, got %+v", got)
	}
}

func TestRuntimeHandlerToolControlsExposeGlobalAndAgentSimplePolicies(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.New(ctx, filepath.Join(t.TempDir(), "runtime-tool-controls-global-agent-simple.db"))
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	defer db.Close()
	st := sqlite.NewStore(db)
	user, err := st.CreateUser(ctx, "runtime-tool-controls-simple-scopes@test.example", "hash", "")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	agent, err := st.CreateAgent(ctx, user.ID, "codex", "token-hash")
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	agentID := agent.ID
	if err := st.CreateRuntimePolicyRule(ctx, &store.RuntimePolicyRule{
		ID:         "global-bash-deny",
		UserID:     user.ID,
		Kind:       "tool",
		Action:     "deny",
		ToolName:   "Bash",
		InputShape: json.RawMessage(`{}`),
		Reason:     "global Bash default",
		Source:     "user",
		Enabled:    true,
	}); err != nil {
		t.Fatalf("CreateRuntimePolicyRule global: %v", err)
	}
	if err := st.CreateRuntimePolicyRule(ctx, &store.RuntimePolicyRule{
		ID:         "agent-bash-allow",
		UserID:     user.ID,
		AgentID:    &agentID,
		Kind:       "tool",
		Action:     "allow",
		ToolName:   "Bash",
		InputShape: json.RawMessage(`{}`),
		Reason:     "agent Bash override",
		Source:     "user",
		Enabled:    true,
	}); err != nil {
		t.Fatalf("CreateRuntimePolicyRule agent: %v", err)
	}

	h := NewRuntimeHandler(st, nil, nil, config.Default(), nil)
	req := httptest.NewRequest(http.MethodGet, "/api/runtime/tool-controls?agent_id="+agent.ID, nil)
	req = req.WithContext(context.WithValue(req.Context(), middleware.UserContextKey, user))
	res := httptest.NewRecorder()
	h.ListToolControls(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("ListToolControls status=%d body=%s", res.Code, res.Body.String())
	}
	var listed struct {
		Entries []runtimeToolControlResponse `json:"entries"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode listed controls: %v", err)
	}
	if len(listed.Entries) != 1 {
		t.Fatalf("expected one Bash control, got %+v", listed.Entries)
	}
	got := listed.Entries[0]
	if got.ToolName != "Bash" || got.Action != "allow" || got.Scope != "agent" {
		t.Fatalf("effective control should remain agent-scoped allow, got %+v", got)
	}
	if got.GlobalAction != "deny" || got.GlobalRuleID != "global-bash-deny" {
		t.Fatalf("global simple policy should be exposed separately, got %+v", got)
	}
	if got.AgentAction != "allow" || got.AgentRuleID != "agent-bash-allow" {
		t.Fatalf("agent simple policy should be exposed separately, got %+v", got)
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
	user, err := st.CreateUser(ctx, "runtime-promote@test.example", "hash", "")
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
