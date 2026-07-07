package llmproxy

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/clawvisor/clawvisor/internal/observability"
	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/inspector"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/pricing"
	"github.com/clawvisor/clawvisor/pkg/store"
	"github.com/clawvisor/clawvisor/pkg/store/sqlite"
)

func newAuditTestStore(t *testing.T) (store.Store, *store.Agent) {
	t.Helper()
	ctx := context.Background()
	db, err := sqlite.New(ctx, filepath.Join(t.TempDir(), "audit.db"))
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := sqlite.NewStore(db)
	user, err := st.CreateUser(ctx, "audit@example.com", "x", "")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	agent, err := st.CreateAgent(ctx, user.ID, "audit-agent", "agent-token-hash")
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	return st, agent
}

func TestAuditEmitter_LogEndpointCall(t *testing.T) {
	st, agent := newAuditTestStore(t)
	em := NewAuditEmitter(st, nil, nil)

	em.LogEndpointCall(context.Background(), agent, "req-1", "anthropic", "lite_proxy.messages.create",
		200, "allow", "success", "", 12*time.Millisecond, map[string]any{"input_tokens": 18, "output_tokens": 8}, EndpointCallExtras{})

	rows, _, err := st.ListAuditEntries(context.Background(), agent.UserID, store.AuditFilter{})
	if err != nil {
		t.Fatalf("ListAuditEntries: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 audit row, got %d", len(rows))
	}
	row := rows[0]
	if row.Service != "anthropic" {
		t.Errorf("service=%q, want anthropic", row.Service)
	}
	if row.Action != "lite_proxy.messages.create" {
		t.Errorf("action=%q", row.Action)
	}
	if row.Decision != "allow" || row.Outcome != "success" {
		t.Errorf("decision/outcome mismatch: %s / %s", row.Decision, row.Outcome)
	}
	if row.AgentID == nil || *row.AgentID != agent.ID {
		t.Errorf("agent_id missing or wrong: %v", row.AgentID)
	}
	if row.DurationMS != 12 {
		t.Errorf("duration_ms=%d, want 12", row.DurationMS)
	}
	var params map[string]any
	if err := json.Unmarshal(row.ParamsSafe, &params); err != nil {
		t.Fatalf("params unmarshal: %v", err)
	}
	if params["input_tokens"] != float64(18) {
		t.Errorf("expected input_tokens=18, got %v", params["input_tokens"])
	}
	if params["http_status"] != float64(200) {
		t.Errorf("expected http_status=200, got %v", params["http_status"])
	}
	if _, ok := params["build_sha"]; !ok {
		t.Errorf("expected forensic build_sha")
	}
	if _, ok := params["parser_version"]; !ok {
		t.Errorf("expected forensic parser_version")
	}
}

func TestAuditEmitter_LogEndpointCallRedactsReasonSecrets(t *testing.T) {
	st, agent := newAuditTestStore(t)
	em := NewAuditEmitter(st, nil, nil)

	em.LogEndpointCall(context.Background(), agent, "req-secret", "anthropic", "lite_proxy.messages.create",
		500, "deny", "upstream_error", "upstream rejected Bearer sk-ant-api03-secret-value", time.Millisecond, nil, EndpointCallExtras{})

	rows, _, err := st.ListAuditEntries(context.Background(), agent.UserID, store.AuditFilter{})
	if err != nil {
		t.Fatalf("ListAuditEntries: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 audit row, got %d", len(rows))
	}
	if rows[0].Reason == nil {
		t.Fatal("reason missing")
	}
	if got := *rows[0].Reason; got == "" || got == "upstream rejected Bearer sk-ant-api03-secret-value" {
		t.Fatalf("reason not redacted: %q", got)
	}
}

// TestAuditEmitter_LogEndpointCall_DedupCostUsesCanonicalAuditID
// pins the FK-safety contract on the dedup path: when LogAudit
// returns ErrConflict (the canonical row already exists for this
// request_id), the cost row must be written against the surviving
// canonical audit row's id — not the locally generated id that
// never landed. With FK llm_request_cost.audit_id REFERENCES
// audit_log(id), using the local id would fail the insert.
func TestAuditEmitter_LogEndpointCall_DedupCostUsesCanonicalAuditID(t *testing.T) {
	st, agent := newAuditTestStore(t)
	em := NewAuditEmitter(st, nil, nil)
	ctx := context.Background()

	usage := &ExtractUsageResult{
		Found: true,
		Model: "claude-opus-4-7",
		Usage: pricing.Usage{InputTokens: 100, OutputTokens: 50},
	}

	// First call lands the canonical audit row + cost row.
	em.LogEndpointCall(ctx, agent, "req-dedup", "anthropic", "lite_proxy.messages.create",
		200, "allow", "success", "", 0, nil, EndpointCallExtras{Usage: usage})

	rows, _, err := st.ListAuditEntries(ctx, agent.UserID, store.AuditFilter{})
	if err != nil {
		t.Fatalf("ListAuditEntries: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 audit row after first call, got %d", len(rows))
	}
	canonicalAuditID := rows[0].ID

	costSummary, err := st.GetTaskCost(ctx, agent.UserID, "")
	if err != nil {
		t.Fatalf("GetTaskCost: %v", err)
	}
	_ = costSummary // task_id is NULL on these rows; just confirm no error

	// Second call with same request_id triggers ErrConflict on LogAudit.
	// The cost record must succeed (FK-safe) by pointing at the
	// canonical row's id, not the new entry's local id.
	em.LogEndpointCall(ctx, agent, "req-dedup", "anthropic", "lite_proxy.messages.create",
		200, "allow", "success", "", 0, nil, EndpointCallExtras{Usage: usage})

	// Still exactly one audit row (dedup worked).
	rows, _, err = st.ListAuditEntries(ctx, agent.UserID, store.AuditFilter{})
	if err != nil {
		t.Fatalf("ListAuditEntries after retry: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 audit row after retry (dedup), got %d", len(rows))
	}
	if rows[0].ID != canonicalAuditID {
		t.Fatalf("canonical audit id changed unexpectedly: %s -> %s", canonicalAuditID, rows[0].ID)
	}

	// The cost row from the FIRST call must still be there (PK conflict
	// on audit_id meant the retry insert was a no-op rather than a row
	// pointing at a non-existent audit_id). If the dedup path had used
	// a fresh entry.ID, the FK would have rejected it; if there were
	// no FK, we'd now have two cost rows for one canonical audit row.
	sqliteStore, ok := st.(*sqlite.Store)
	if !ok {
		t.Fatalf("expected *sqlite.Store, got %T", st)
	}
	db := sqliteStore.DB()
	var costRowCount int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM llm_request_cost WHERE audit_id = ?`, canonicalAuditID,
	).Scan(&costRowCount); err != nil {
		t.Fatalf("count cost rows: %v", err)
	}
	if costRowCount != 1 {
		t.Fatalf("expected exactly 1 cost row tied to canonical audit_id %s, got %d",
			canonicalAuditID, costRowCount)
	}

	// And no orphan cost rows pointing at audit ids that don't exist.
	var orphans int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM llm_request_cost c
		 LEFT JOIN audit_log a ON a.id = c.audit_id
		 WHERE a.id IS NULL`,
	).Scan(&orphans); err != nil {
		t.Fatalf("count orphan cost rows: %v", err)
	}
	if orphans != 0 {
		t.Fatalf("found %d orphan cost rows pointing at non-existent audit_log rows", orphans)
	}
}

// TestAuditEmitter_LogEndpointCall_MetricsNotDoubleCountedOnDedup pins the
// fix that token/cost metrics are recorded only after RecordLLMRequestCost
// confirms the insert. A dedup retry (same request_id) hits ErrConflict on
// both the audit insert and the cost insert, so its tokens/cost must NOT be
// counted again — otherwise every dedup retry double-counts usage.
func TestAuditEmitter_LogEndpointCall_MetricsNotDoubleCountedOnDedup(t *testing.T) {
	st, agent := newAuditTestStore(t)
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	inst, err := observability.NewInstruments(mp.Meter("test"))
	if err != nil {
		t.Fatalf("NewInstruments: %v", err)
	}
	em := NewAuditEmitter(st, nil, nil)
	em.Instruments = inst
	ctx := context.Background()

	usage := &ExtractUsageResult{
		Found: true,
		Model: "claude-opus-4-7",
		Usage: pricing.Usage{InputTokens: 100, OutputTokens: 50},
	}

	// First call lands the canonical audit + cost row and records metrics.
	em.LogEndpointCall(ctx, agent, "req-metric-dedup", "anthropic", "lite_proxy.messages.create",
		200, "allow", "success", "", 0, nil, EndpointCallExtras{Usage: usage})
	// Second call: same request_id → ErrConflict on both audit and cost
	// inserts. Metrics must not be re-recorded.
	em.LogEndpointCall(ctx, agent, "req-metric-dedup", "anthropic", "lite_proxy.messages.create",
		200, "allow", "success", "", 0, nil, EndpointCallExtras{Usage: usage})

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}

	tokensByDir := map[string]int64{}
	var costMicros int64
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			switch m.Name {
			case observability.MetricLLMTokens:
				sum, ok := m.Data.(metricdata.Sum[int64])
				if !ok {
					t.Fatalf("%s not int64 sum: %T", m.Name, m.Data)
				}
				for _, dp := range sum.DataPoints {
					dir, _ := dp.Attributes.Value(observability.AttrDirection)
					tokensByDir[dir.AsString()] += dp.Value
				}
			case observability.MetricLLMCostUSDMicros:
				sum, ok := m.Data.(metricdata.Sum[int64])
				if !ok {
					t.Fatalf("%s not int64 sum: %T", m.Name, m.Data)
				}
				for _, dp := range sum.DataPoints {
					costMicros += dp.Value
				}
			}
		}
	}

	// Exactly one call's worth of tokens — not two.
	if tokensByDir["input"] != 100 {
		t.Errorf("input tokens = %d, want 100 (double-counted on dedup retry?)", tokensByDir["input"])
	}
	if tokensByDir["output"] != 50 {
		t.Errorf("output tokens = %d, want 50 (double-counted on dedup retry?)", tokensByDir["output"])
	}
	// Cost, if the model is priced, must reflect a single call.
	wantCost := pricing.Compute(pricing.Normalize(usage.Model), usage.Usage)
	if wantCost.Known && costMicros != wantCost.CostMicros {
		t.Errorf("cost micros = %d, want %d (single call)", costMicros, wantCost.CostMicros)
	}
}

func TestAuditEmitter_LogToolUseInspected(t *testing.T) {
	st, agent := newAuditTestStore(t)
	em := NewAuditEmitter(st, nil, nil)

	verdict := inspector.Verdict{
		IsAPICall:    true,
		Method:       "POST",
		Host:         "api.github.com",
		Path:         "/repos/x/y/issues",
		Source:       inspector.SourceDeterministic,
		Reason:       "structured fetch with header credential",
		Placeholders: []string{"autovault_github_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"},
		CredentialLocations: []inspector.CredentialLocation{
			{Kind: "header", Name: "Authorization", Scheme: "Bearer"},
		},
	}
	em.LogToolUseInspected(context.Background(), agent, "req-1", conversation.ToolUse{
		ID:    "toolu_1",
		Name:  "WebFetch",
		Input: json.RawMessage(`{"url":"https://api.github.com/repos/x/y/issues","headers":{"Authorization":"Bearer secret"}}`),
	}, verdict, "rewrite", "success", verdict.Reason, "")

	rows, _, _ := st.ListAuditEntries(context.Background(), agent.UserID, store.AuditFilter{})
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	row := rows[0]
	if row.Service != "runtime.tool_use" {
		t.Errorf("service=%q, want runtime.tool_use", row.Service)
	}
	if row.Action != "lite_proxy.tool_use.rewrite" {
		t.Errorf("action=%q", row.Action)
	}
	if row.ToolUseID == nil || *row.ToolUseID != "toolu_1" {
		t.Errorf("tool_use_id missing or wrong: %v", row.ToolUseID)
	}
	if row.RequestID != "req-1" {
		t.Errorf("request_id=%q, want original parent request id", row.RequestID)
	}
	if row.DedupKey == nil || *row.DedupKey == "" {
		t.Errorf("dedup_key missing on lite-proxy tool-use row")
	}
	if row.TaskID != nil {
		t.Errorf("task_id should be nil when caller passes empty string, got %v", row.TaskID)
	}
	var params map[string]any
	_ = json.Unmarshal(row.ParamsSafe, &params)
	if params["parent_request_id"] != "req-1" {
		t.Errorf("expected parent_request_id=req-1, got %v", params["parent_request_id"])
	}
	if params["tool_use_id"] != "toolu_1" {
		t.Errorf("expected params tool_use_id=toolu_1, got %v", params["tool_use_id"])
	}
	if params["target_host"] != "api.github.com" {
		t.Errorf("expected target_host in params, got %v", params["target_host"])
	}
	if params["verdict_source"] != "deterministic" {
		t.Errorf("expected verdict_source=deterministic, got %v", params["verdict_source"])
	}
	if params["tool_name"] != "WebFetch" {
		t.Errorf("expected tool_name=WebFetch, got %v", params["tool_name"])
	}
	if params["tool_target"] != "https://api.github.com/repos/x/y/issues" {
		t.Errorf("expected tool_target URL, got %v", params["tool_target"])
	}
	toolInput, _ := params["tool_input"].(map[string]any)
	headers, _ := toolInput["headers"].(map[string]any)
	if _, ok := headers["Authorization"]; ok {
		t.Errorf("tool_input should not persist Authorization header: %+v", headers)
	}
}

// TestAuditEmitter_WriteAuditEvent_ScriptSessionJudgeForensics
// confirms that judge invocation forensics on a ScriptSessionFact
// (prompt SHA, latency, token usage, error) round-trip into the
// audit row's ParamsSafe JSON. Without persistence, operators can't
// roll up judge cost or investigate flaky judge calls from the
// audit store alone.
func TestAuditEmitter_WriteAuditEvent_ScriptSessionJudgeForensics(t *testing.T) {
	st, agent := newAuditTestStore(t)
	em := NewAuditEmitter(st, nil, nil)

	em.WriteAuditEvent(context.Background(), agent, "req-1", conversation.AuditEvent{
		ToolUse: conversation.ToolUse{
			ID:    "toolu_judge",
			Name:  "Bash",
			Input: json.RawMessage(`{"command":"curl http://localhost:25297/api/proxy/x"}`),
		},
		Decision:    conversation.DecisionAllow,
		OutcomeName: "script_session_judge_allow",
		Reason:      "variable holds the resolver URL",
		Facts: []conversation.EvaluationFact{
			conversation.ScriptSessionFact{
				Outcome:           "script_session_judge_allow",
				JudgePromptSHA:    "abc123def456",
				JudgeLatencyMS:    47,
				JudgeInputTokens:  1234,
				JudgeOutputTokens: 56,
			},
		},
	})

	rows, _, _ := st.ListAuditEntries(context.Background(), agent.UserID, store.AuditFilter{})
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	var params map[string]any
	if err := json.Unmarshal(rows[0].ParamsSafe, &params); err != nil {
		t.Fatalf("unmarshal ParamsSafe: %v", err)
	}
	if params["script_session_judge_prompt_sha"] != "abc123def456" {
		t.Errorf("prompt_sha = %v, want abc123def456", params["script_session_judge_prompt_sha"])
	}
	// JSON unmarshals numbers as float64.
	if v, _ := params["script_session_judge_latency_ms"].(float64); v != 47 {
		t.Errorf("latency_ms = %v, want 47", params["script_session_judge_latency_ms"])
	}
	if v, _ := params["script_session_judge_input_tokens"].(float64); v != 1234 {
		t.Errorf("input_tokens = %v, want 1234", params["script_session_judge_input_tokens"])
	}
	if v, _ := params["script_session_judge_output_tokens"].(float64); v != 56 {
		t.Errorf("output_tokens = %v, want 56", params["script_session_judge_output_tokens"])
	}
	if _, present := params["script_session_judge_error"]; present {
		t.Errorf("judge_error should be absent on allow path, got %v", params["script_session_judge_error"])
	}
}

// TestAuditEmitter_WriteAuditEvent_AnnotationFacts confirms that
// judge forensics surface even when the script_session evaluator
// yielded (Skip) and another evaluator claimed the tool_use. The
// orchestrator records non-winning forensic facts on
// AnnotationFacts; WriteAuditEvent walks them alongside Facts. Without
// this, judge_error rows would only appear in the structured logger,
// not the audit DB.
func TestAuditEmitter_WriteAuditEvent_AnnotationFacts(t *testing.T) {
	st, agent := newAuditTestStore(t)
	em := NewAuditEmitter(st, nil, nil)

	em.WriteAuditEvent(context.Background(), agent, "req-1", conversation.AuditEvent{
		ToolUse: conversation.ToolUse{ID: "toolu_judge_err"},
		// Winning event is from a different evaluator (e.g. inspector
		// chain refused). The script_session evaluator's judge_error
		// is on AnnotationFacts.
		Decision:    conversation.DecisionBlock,
		OutcomeName: "boundary_check_failed",
		AnnotationFacts: []conversation.EvaluationFact{
			conversation.ScriptSessionFact{
				Outcome:        "script_session_judge_error",
				JudgePromptSHA: "annot_sha",
				JudgeLatencyMS: 42,
				JudgeError:     "scriptjudge transport: timeout",
			},
		},
	})

	rows, _, _ := st.ListAuditEntries(context.Background(), agent.UserID, store.AuditFilter{})
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	var params map[string]any
	_ = json.Unmarshal(rows[0].ParamsSafe, &params)
	if params["script_session_judge_prompt_sha"] != "annot_sha" {
		t.Errorf("AnnotationFacts prompt_sha not surfaced: %v", params["script_session_judge_prompt_sha"])
	}
	if v, _ := params["script_session_judge_latency_ms"].(float64); v != 42 {
		t.Errorf("AnnotationFacts latency_ms not surfaced: %v", params["script_session_judge_latency_ms"])
	}
	if msg, _ := params["script_session_judge_error"].(string); !strings.Contains(msg, "scriptjudge transport") {
		t.Errorf("AnnotationFacts judge_error not surfaced: %v", params["script_session_judge_error"])
	}
}

// TestAuditEmitter_WriteAuditEvent_ScriptSessionJudgeError confirms
// that a judge-error outcome surfaces the error string into
// ParamsSafe — operators need to see transient transport failures
// without re-reading proxy logs.
func TestAuditEmitter_WriteAuditEvent_ScriptSessionJudgeError(t *testing.T) {
	st, agent := newAuditTestStore(t)
	em := NewAuditEmitter(st, nil, nil)

	em.WriteAuditEvent(context.Background(), agent, "req-1", conversation.AuditEvent{
		ToolUse:     conversation.ToolUse{ID: "toolu_err", Name: "Bash"},
		Decision:    conversation.DecisionAllow, // Skip = chain continues; decision recorded by later stage
		OutcomeName: "script_session_judge_error",
		Facts: []conversation.EvaluationFact{
			conversation.ScriptSessionFact{
				Outcome:        "script_session_judge_error",
				JudgePromptSHA: "abc123",
				JudgeLatencyMS: 31,
				JudgeError:     "scriptjudge transport: connection reset",
			},
		},
	})

	rows, _, _ := st.ListAuditEntries(context.Background(), agent.UserID, store.AuditFilter{})
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	var params map[string]any
	_ = json.Unmarshal(rows[0].ParamsSafe, &params)
	msg, _ := params["script_session_judge_error"].(string)
	if msg == "" {
		t.Fatalf("script_session_judge_error not persisted: %v", params)
	}
	// auditErrorDetail may truncate, but the core message should survive.
	if !strings.Contains(msg, "scriptjudge transport") {
		t.Errorf("error detail %q should contain core message", msg)
	}
}

// TestAuditEmitter_LogToolUseInspected_TaskID confirms that passing a
// non-empty taskID populates AuditEntry.TaskID — the field the
// dashboard's per-task activity feed filters on (GET /api/audit?task_id=).
// Without this linkage, lite-proxy tool_use rows never appear in any
// task's activity tab.
func TestAuditEmitter_LogToolUseInspected_TaskID(t *testing.T) {
	st, agent := newAuditTestStore(t)
	em := NewAuditEmitter(st, nil, nil)

	em.LogToolUseInspected(context.Background(), agent, "req-task", conversation.ToolUse{
		ID:    "toolu_2",
		Name:  "WebFetch",
		Input: json.RawMessage(`{"url":"https://api.github.com/repos/x/y"}`),
	}, inspector.Verdict{IsAPICall: true, Host: "api.github.com", Method: "GET", Path: "/repos/x/y", Source: inspector.SourceDeterministic},
		"allow", "matched_task", "scope covered", "task-abc")

	rows, _, _ := st.ListAuditEntries(context.Background(), agent.UserID, store.AuditFilter{TaskID: "task-abc"})
	if len(rows) != 1 {
		t.Fatalf("expected 1 row filtered by task_id, got %d", len(rows))
	}
	if rows[0].TaskID == nil || *rows[0].TaskID != "task-abc" {
		t.Errorf("task_id not persisted: %v", rows[0].TaskID)
	}
}

func TestAuditEmitter_LogToolUseInspected_AllowsMultipleRowsPerParentRequestAndTask(t *testing.T) {
	st, agent := newAuditTestStore(t)
	em := NewAuditEmitter(st, nil, nil)
	ctx := context.Background()
	verdict := inspector.Verdict{
		IsAPICall: true,
		Host:      "api.github.com",
		Method:    "GET",
		Path:      "/repos/x/y",
		Source:    inspector.SourceDeterministic,
	}

	em.LogToolUseInspected(ctx, agent, "req-multi", conversation.ToolUse{
		ID:    "toolu_one",
		Name:  "WebFetch",
		Input: json.RawMessage(`{"url":"https://api.github.com/repos/x/y"}`),
	}, verdict, "allow", "matched_task", "scope covered", "task-multi")
	em.LogToolUseInspected(ctx, agent, "req-multi", conversation.ToolUse{
		ID:    "toolu_two",
		Name:  "WebFetch",
		Input: json.RawMessage(`{"url":"https://api.github.com/repos/x/y/issues"}`),
	}, verdict, "allow", "matched_task", "scope covered", "task-multi")
	em.LogToolUseInspected(ctx, agent, "req-multi", conversation.ToolUse{
		ID:    "toolu_two",
		Name:  "WebFetch",
		Input: json.RawMessage(`{"url":"https://api.github.com/repos/x/y/issues"}`),
	}, verdict, "allow", "matched_task", "scope covered", "task-multi")

	rows, _, err := st.ListAuditEntries(ctx, agent.UserID, store.AuditFilter{TaskID: "task-multi"})
	if err != nil {
		t.Fatalf("ListAuditEntries: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 tool-use rows with same parent request/task, got %d", len(rows))
	}
	got := map[string]bool{}
	for _, row := range rows {
		if row.ToolUseID == nil {
			t.Fatalf("tool_use_id missing on row: %+v", row)
		}
		got[*row.ToolUseID] = true
		if row.RequestID != "req-multi" {
			t.Fatalf("request_id=%q, want original parent request id", row.RequestID)
		}
		var params map[string]any
		if err := json.Unmarshal(row.ParamsSafe, &params); err != nil {
			t.Fatalf("params unmarshal: %v", err)
		}
		if params["parent_request_id"] != "req-multi" {
			t.Fatalf("parent_request_id=%v, want req-multi", params["parent_request_id"])
		}
		if params["tool_use_id"] != *row.ToolUseID {
			t.Fatalf("params tool_use_id=%v, want %s", params["tool_use_id"], *row.ToolUseID)
		}
	}
	if !got["toolu_one"] || !got["toolu_two"] {
		t.Fatalf("missing expected tool-use rows: %+v", got)
	}
}

func TestAuditLog_RuntimeToolUseDeduplicatesRepeatedChildEvents(t *testing.T) {
	st, agent := newAuditTestStore(t)
	ctx := context.Background()
	taskID := "task-dedup"
	toolUseID := "toolu_repeat"
	agentID := agent.ID

	entry := func(id, dedupKey string) *store.AuditEntry {
		return &store.AuditEntry{
			ID:         id,
			UserID:     agent.UserID,
			AgentID:    &agentID,
			RequestID:  "req-dedup",
			DedupKey:   &dedupKey,
			TaskID:     &taskID,
			ToolUseID:  &toolUseID,
			Timestamp:  time.Now().UTC(),
			Service:    "runtime.tool_use",
			Action:     "lite_proxy.tool_use.allow",
			ParamsSafe: json.RawMessage(`{}`),
			Decision:   "allow",
			Outcome:    "matched_task",
		}
	}

	if err := st.LogAudit(ctx, entry("audit-one", "lite_proxy_event:req-dedup:toolu_repeat")); err != nil {
		t.Fatalf("LogAudit(first): %v", err)
	}
	if err := st.LogAudit(ctx, entry("audit-two", "lite_proxy_event:req-dedup:toolu_repeat")); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("LogAudit(repeated child event) = %v, want ErrConflict", err)
	}

	siblingToolUseID := "toolu_sibling"
	sibling := entry("audit-three", "lite_proxy_event:req-dedup:toolu_sibling")
	sibling.ToolUseID = &siblingToolUseID
	if err := st.LogAudit(ctx, sibling); err != nil {
		t.Fatalf("LogAudit(sibling child): %v", err)
	}
}

func TestAuditLog_LiteProxyChildrenPreserveEventHistory(t *testing.T) {
	st, agent := newAuditTestStore(t)
	ctx := context.Background()
	agentID := agent.ID

	base := &store.AuditEntry{
		ID:         "audit-endpoint",
		UserID:     agent.UserID,
		AgentID:    &agentID,
		RequestID:  "req-lite-children",
		Timestamp:  time.Now().UTC(),
		Service:    "anthropic",
		Action:     "lite_proxy.messages.create",
		ParamsSafe: json.RawMessage(`{"event":"lite_proxy.endpoint_call"}`),
		Decision:   "allow",
		Outcome:    "success",
	}
	if err := st.LogAudit(ctx, base); err != nil {
		t.Fatalf("LogAudit(endpoint): %v", err)
	}
	duplicateEndpoint := *base
	duplicateEndpoint.ID = "audit-endpoint-dupe"
	if err := st.LogAudit(ctx, &duplicateEndpoint); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("LogAudit(duplicate request-level endpoint) = %v, want ErrConflict", err)
	}

	resolver := *base
	resolver.ID = "audit-resolver"
	resolverDedupKey := "lite_proxy_event:audit-resolver"
	resolver.DedupKey = &resolverDedupKey
	resolver.Service = "github"
	resolver.Action = "lite_proxy.resolver.POST"
	resolver.Outcome = "resolved"
	resolver.ParamsSafe = json.RawMessage(`{"event":"lite_proxy.resolver_swap","placeholder":"autovault_github_x","target_host":"api.github.com","target_path":"/repos/x/y"}`)
	if err := st.LogAudit(ctx, &resolver); err != nil {
		t.Fatalf("LogAudit(resolver child): %v", err)
	}
	duplicateResolver := resolver
	duplicateResolver.ID = "audit-resolver-dupe"
	if err := st.LogAudit(ctx, &duplicateResolver); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("LogAudit(repeated resolver child event) = %v, want ErrConflict", err)
	}

	secondResolver := resolver
	secondResolver.ID = "audit-resolver-second"
	secondResolverDedupKey := "lite_proxy_event:audit-resolver-second"
	secondResolver.DedupKey = &secondResolverDedupKey
	secondResolver.ParamsSafe = json.RawMessage(`{"event":"lite_proxy.resolver_swap","placeholder":"autovault_github_y","target_host":"api.github.com","target_path":"/repos/x/y/issues"}`)
	if err := st.LogAudit(ctx, &secondResolver); err != nil {
		t.Fatalf("LogAudit(second resolver child): %v", err)
	}
}

func TestAuditLog_RequestLookupPrefersRequestLevelRows(t *testing.T) {
	st, agent := newAuditTestStore(t)
	ctx := context.Background()
	agentID := agent.ID
	requestLevel := &store.AuditEntry{
		ID:         "audit-request",
		UserID:     agent.UserID,
		AgentID:    &agentID,
		RequestID:  "req-lookup",
		Timestamp:  time.Now().UTC(),
		Service:    "anthropic",
		Action:     "lite_proxy.messages.create",
		ParamsSafe: json.RawMessage(`{"event":"lite_proxy.endpoint_call"}`),
		Decision:   "allow",
		Outcome:    "success",
	}
	if err := st.LogAudit(ctx, requestLevel); err != nil {
		t.Fatalf("LogAudit(request-level): %v", err)
	}
	childDedupKey := "lite_proxy_event:audit-child"
	toolUseID := "toolu_child"
	child := &store.AuditEntry{
		ID:         "audit-child",
		UserID:     agent.UserID,
		AgentID:    &agentID,
		RequestID:  "req-lookup",
		DedupKey:   &childDedupKey,
		ToolUseID:  &toolUseID,
		Timestamp:  time.Now().UTC().Add(time.Second),
		Service:    "runtime.tool_use",
		Action:     "lite_proxy.tool_use.block",
		ParamsSafe: json.RawMessage(`{"event":"lite_proxy.tool_use_inspected"}`),
		Decision:   "block",
		Outcome:    "task_scope_missing",
	}
	if err := st.LogAudit(ctx, child); err != nil {
		t.Fatalf("LogAudit(child): %v", err)
	}

	got, err := st.GetAuditEntryByRequestID(ctx, "req-lookup", agent.UserID)
	if err != nil {
		t.Fatalf("GetAuditEntryByRequestID: %v", err)
	}
	if got.ID != requestLevel.ID {
		t.Fatalf("GetAuditEntryByRequestID returned %s, want request-level row %s", got.ID, requestLevel.ID)
	}
	got, err = st.FindDedupCandidate(ctx, "req-lookup", agent.UserID, "")
	if err != nil {
		t.Fatalf("FindDedupCandidate: %v", err)
	}
	if got.ID != requestLevel.ID {
		t.Fatalf("FindDedupCandidate returned %s, want request-level row %s", got.ID, requestLevel.ID)
	}
}

func TestAuditLog_FindDedupCandidateUsesExactTaskRequestLevelRow(t *testing.T) {
	st, agent := newAuditTestStore(t)
	ctx := context.Background()
	agentID := agent.ID
	preTask := &store.AuditEntry{
		ID:         "audit-pre-task",
		UserID:     agent.UserID,
		AgentID:    &agentID,
		RequestID:  "req-task-precedence",
		Timestamp:  time.Now().UTC(),
		Service:    "anthropic",
		Action:     "lite_proxy.messages.create",
		ParamsSafe: json.RawMessage(`{"event":"lite_proxy.endpoint_call"}`),
		Decision:   "allow",
		Outcome:    "pre_task",
	}
	if err := st.LogAudit(ctx, preTask); err != nil {
		t.Fatalf("LogAudit(pre-task): %v", err)
	}
	taskID := "task-exact"
	taskScoped := &store.AuditEntry{
		ID:         "audit-task-scoped",
		UserID:     agent.UserID,
		AgentID:    &agentID,
		RequestID:  "req-task-precedence",
		TaskID:     &taskID,
		Timestamp:  time.Now().UTC().Add(time.Second),
		Service:    "anthropic",
		Action:     "lite_proxy.messages.create",
		ParamsSafe: json.RawMessage(`{"event":"lite_proxy.endpoint_call"}`),
		Decision:   "allow",
		Outcome:    "task_scoped",
	}
	if err := st.LogAudit(ctx, taskScoped); err != nil {
		t.Fatalf("LogAudit(task-scoped): %v", err)
	}
	childDedupKey := "lite_proxy_event:req-task-precedence:toolu_child"
	child := *taskScoped
	child.ID = "audit-child-keyed"
	child.DedupKey = &childDedupKey
	child.Service = "runtime.tool_use"
	child.Action = "lite_proxy.tool_use.allow"
	if err := st.LogAudit(ctx, &child); err != nil {
		t.Fatalf("LogAudit(child): %v", err)
	}

	got, err := st.FindDedupCandidate(ctx, "req-task-precedence", agent.UserID, taskID)
	if err != nil {
		t.Fatalf("FindDedupCandidate: %v", err)
	}
	if got.ID != taskScoped.ID {
		t.Fatalf("FindDedupCandidate returned %s, want exact task row %s", got.ID, taskScoped.ID)
	}
}

func TestAuditEmitter_LogResolverSwap(t *testing.T) {
	st, agent := newAuditTestStore(t)
	em := NewAuditEmitter(st, nil, nil)

	em.LogResolverSwap(context.Background(), agent, "req-2",
		"autovault_github_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx", "github", "api.github.com", "/repos/x/y", "POST",
		201, "allow", "success", "",
		7*time.Millisecond)

	rows, _, _ := st.ListAuditEntries(context.Background(), agent.UserID, store.AuditFilter{})
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	row := rows[0]
	if row.Service != "github" {
		t.Errorf("service=%q, want github", row.Service)
	}
	if row.Action != "lite_proxy.resolver.POST" {
		t.Errorf("action=%q", row.Action)
	}
	if row.DurationMS != 7 {
		t.Errorf("duration_ms=%d, want 7", row.DurationMS)
	}
	var params map[string]any
	_ = json.Unmarshal(row.ParamsSafe, &params)
	if params["target_host"] != "api.github.com" {
		t.Errorf("expected target_host=api.github.com, got %v", params["target_host"])
	}
	if params["placeholder"] != "autovault_github_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx" {
		t.Errorf("expected placeholder in params, got %v", params["placeholder"])
	}
}

// PromptSHA stub used to wire forensic field plumbing.
type promptSHAStub struct{ sha string }

func (p promptSHAStub) PromptSHA() string { return p.sha }

func TestAuditEmitter_PopulatesValidatorPromptSHA(t *testing.T) {
	st, agent := newAuditTestStore(t)
	em := NewAuditEmitter(st, nil, promptSHAStub{sha: "abc123"})

	em.LogEndpointCall(context.Background(), agent, "req-1", "anthropic", "lite_proxy.messages.create",
		200, "allow", "success", "", 0, nil, EndpointCallExtras{})

	rows, _, _ := st.ListAuditEntries(context.Background(), agent.UserID, store.AuditFilter{})
	if len(rows) != 1 {
		t.Fatalf("rows=%d", len(rows))
	}
	var params map[string]any
	_ = json.Unmarshal(rows[0].ParamsSafe, &params)
	if params["validator_prompt"] != "abc123" {
		t.Errorf("expected validator_prompt=abc123, got %v", params["validator_prompt"])
	}
}

// Security: credentials embedded in audit values (not just keys) must
// be redacted. A `command` value containing `Bearer ghp_...` previously
// landed verbatim in the audit row.
func TestRedactSecretsInString(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "bearer_token_in_command",
			in:   "curl -H 'Authorization: Bearer ghp_realtoken123' https://api.github.com/x",
			want: "curl -H 'Authorization: <REDACTED:auth>' https://api.github.com/x",
		},
		{
			name: "anthropic_key",
			in:   "use sk-ant-realsecret as the key",
			want: "use <REDACTED:auth> as the key",
		},
		{
			name: "openai_key",
			in:   "OPENAI_API_KEY=sk-proj-realsecret123",
			want: "OPENAI_API_KEY=<REDACTED:auth>",
		},
		{
			name: "agent_token",
			in:   "agent token is cvis_abc123def",
			want: "agent token is <REDACTED:auth>",
		},
		{
			name: "url_basic_auth",
			in:   "https://user:secretpw@api.example.com/path",
			want: "https://<REDACTED:auth>@api.example.com/path",
		},
		{
			name: "autovault_placeholder_survives",
			in:   "use placeholder autovault_github_xyz123 here",
			want: "use placeholder autovault_github_xyz123 here",
		},
		{
			name: "github_fine_grained_pat",
			in:   "TOKEN=github_pat_11ABCDEF0_realfinegrainedpatsecret",
			want: "TOKEN=<REDACTED:auth>",
		},
		{
			name: "github_refresh_token",
			in:   "refresh=ghr_realgithubrefreshtoken123",
			want: "refresh=<REDACTED:auth>",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := redactSecretsInString(tc.in)
			if got != tc.want {
				t.Errorf("redaction:\n  in=  %q\n  got= %q\n  want=%q", tc.in, got, tc.want)
			}
		})
	}
}
