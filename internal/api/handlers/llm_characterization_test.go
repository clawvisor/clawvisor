package handlers

// Characterization tests for the LLM proxy handler.
//
// These tests capture the *current* observable behavior of the handler as
// JSON snapshots in testdata/llm_characterization/. The refactor described
// in .context/llmproxy-refactor-plan.md must preserve these snapshots (or
// explicitly update them with a §10 behavior-change entry).
//
// To regenerate a snapshot intentionally:
//     UPDATE_LLM_CHARACTERIZATION=1 go test ./internal/api/handlers/ -run TestCharacterization
//
// Each scenario runs a representative request through the handler, captures
// the resulting audit row(s), normalizes non-deterministic fields, and
// compares against a golden file. Failures show the unified diff.

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/clawvisor/clawvisor/internal/api/middleware"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// characterizationScenario describes one captured scenario.
type characterizationScenario struct {
	// name doubles as the golden filename stem (testdata/llm_characterization/<name>.json).
	name string
	// upstreamHandler models the upstream LLM provider for this scenario.
	upstreamHandler http.HandlerFunc
	// request builds the inbound HTTP request — token is injected by the runner.
	request func(t *testing.T, rawToken string) *http.Request
	// configure runs before the scenario fires, after newSeededHandler returns.
	// Use this to set ControlBaseURL, Inspector, AuditEmitter overrides, etc.
	configure func(t *testing.T, h *LLMEndpointHandler)
}

// runCharacterizationScenario executes one scenario and returns the captured
// audit rows normalized to a stable shape for snapshotting.
func runCharacterizationScenario(t *testing.T, sc characterizationScenario) []normalizedAuditRow {
	t.Helper()

	upstream := httptest.NewServer(sc.upstreamHandler)
	defer upstream.Close()

	h, st, rawToken, _ := newSeededHandler(t, upstream.URL)
	h.AuditEmitter = llmproxy.NewAuditEmitter(st, slog.Default(), nil)
	if sc.configure != nil {
		sc.configure(t, h)
	}

	mux := http.NewServeMux()
	mw := middleware.RequireAgentLLM(st)
	mux.Handle("POST /v1/messages", mw(http.HandlerFunc(h.Messages)))
	mux.Handle("POST /v1/chat/completions", mw(http.HandlerFunc(h.ChatCompletions)))
	mux.Handle("POST /v1/responses", mw(http.HandlerFunc(h.Responses)))

	req := sc.request(t, rawToken)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	// Drain response body so deferred cleanups complete; status is captured
	// by the audit row itself, not asserted here.
	_, _ = io.Copy(io.Discard, rec.Body)

	user, err := st.GetUserByEmail(context.Background(), "lite-proxy@example.com")
	if err != nil {
		t.Fatalf("GetUserByEmail: %v", err)
	}
	rows, _, err := st.ListAuditEntries(context.Background(), user.ID, store.AuditFilter{})
	if err != nil {
		t.Fatalf("ListAuditEntries: %v", err)
	}

	out := make([]normalizedAuditRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, normalizeAuditRow(*row))
	}
	return out
}

// normalizedAuditRow is the snapshot-friendly subset of store.AuditEntry.
// Non-deterministic fields (IDs, timestamps, byte counts that depend on
// upstream URL length, durations) are dropped or replaced with stable
// placeholders. Params keys are sorted at JSON serialization time.
type normalizedAuditRow struct {
	Action               string                 `json:"action"`
	Decision             string                 `json:"decision"`
	Outcome              string                 `json:"outcome"`
	UsedActiveTaskContext bool                  `json:"used_active_task_context"`
	UsedLeaseBias        bool                   `json:"used_lease_bias"`
	HasApprovalID        bool                   `json:"has_approval_id,omitempty"`
	HasTaskID            bool                   `json:"has_task_id,omitempty"`
	HasLeaseID           bool                   `json:"has_lease_id,omitempty"`
	HasMatchedTaskID     bool                   `json:"has_matched_task_id,omitempty"`
	HasIntentVerdict     bool                   `json:"has_intent_verdict,omitempty"`
	HasPolicyID          bool                   `json:"has_policy_id,omitempty"`
	HasRuleID            bool                   `json:"has_rule_id,omitempty"`
	HasResolutionConfidence bool                `json:"has_resolution_confidence,omitempty"`
	Params               map[string]any         `json:"params"`
}

// normalizeAuditRow strips fields whose values can't be reproduced across
// runs (UUIDs, time, durations) while preserving everything that pins
// policy behavior.
func normalizeAuditRow(row store.AuditEntry) normalizedAuditRow {
	out := normalizedAuditRow{
		Action:                row.Action,
		Decision:              row.Decision,
		Outcome:               row.Outcome,
		UsedActiveTaskContext: row.UsedActiveTaskContext,
		UsedLeaseBias:         row.UsedLeaseBias,
		HasApprovalID:         row.ApprovalID != nil,
		HasTaskID:             row.TaskID != nil,
		HasLeaseID:            row.LeaseID != nil,
		HasMatchedTaskID:      row.MatchedTaskID != nil,
		HasIntentVerdict:      row.IntentVerdict != nil,
		HasPolicyID:           row.PolicyID != nil,
		HasRuleID:             row.RuleID != nil,
		HasResolutionConfidence: row.ResolutionConfidence != nil,
	}

	if len(row.ParamsSafe) > 0 {
		var raw map[string]any
		if err := json.Unmarshal(row.ParamsSafe, &raw); err == nil {
			out.Params = normalizeParams(raw)
		}
	}
	return out
}

// normalizeParams replaces non-deterministic param values with stable
// placeholders so the snapshot is reproducible. The KEYS are what pin
// behavior (which policies ran); the VALUES often carry timing/sizing
// noise we don't want to compare.
func normalizeParams(p map[string]any) map[string]any {
	out := make(map[string]any, len(p))
	for k, v := range p {
		switch k {
		case
			// Timing — varies per run.
			"ttfb_headers_ms",
			"upstream_read_ms",
			"upstream_ttfb_body_ms",
			"continuation_ttfb_headers_ms",
			"continuation_upstream_read_ms",
			"continuation_upstream_ttfb_body_ms",
			// Sizes — depend on upstream URL length and other test infra.
			"request_body_bytes",
			"upstream_body_bytes",
			// Conversation ID — minted by crypto/rand for OpenAI Chat
			// first-turn requests.
			"conversation_id":
			out[k] = "<normalized>"
		default:
			out[k] = v
		}
	}
	return out
}

// compareToGolden compares the normalized rows against testdata/llm_characterization/<name>.json.
// Set UPDATE_LLM_CHARACTERIZATION=1 to regenerate after intentional changes.
func compareToGolden(t *testing.T, name string, rows []normalizedAuditRow) {
	t.Helper()

	got, err := json.MarshalIndent(rows, "", "  ")
	if err != nil {
		t.Fatalf("marshal rows: %v", err)
	}
	// Force a trailing newline so the on-disk file is POSIX-friendly.
	got = append(got, '\n')

	path := filepath.Join("testdata", "llm_characterization", name+".json")

	if os.Getenv("UPDATE_LLM_CHARACTERIZATION") == "1" {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Logf("wrote golden: %s", path)
		return
	}

	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf(
			"read golden %s: %v\n\nThis scenario has no recorded baseline. Generate one with:\n\tUPDATE_LLM_CHARACTERIZATION=1 go test ./internal/api/handlers/ -run %s",
			path, err, t.Name(),
		)
	}

	if string(want) != string(got) {
		t.Fatalf("characterization snapshot mismatch for %s\n--- want ---\n%s\n--- got ---\n%s\n\nIf this change is intentional, regenerate with:\n\tUPDATE_LLM_CHARACTERIZATION=1 go test ./internal/api/handlers/ -run %s",
			path, string(want), string(got), t.Name())
	}
}

// --- Scenarios ---------------------------------------------------------

// TestCharacterization_PassthroughCleanAnthropic pins the audit row produced
// by the simplest possible request — Anthropic Messages with no tool use,
// no inline approvals in flight, no secrets. This snapshot is the canary
// for any pre-phase reordering: every preprocess flag that *would* have
// fired should remain absent (clean baseline).
func TestCharacterization_PassthroughCleanAnthropic(t *testing.T) {
	rows := runCharacterizationScenario(t, characterizationScenario{
		name: "passthrough_clean_anthropic",
		upstreamHandler: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"model":"claude-haiku-4-5","stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":2}}`))
		},
		request: func(t *testing.T, rawToken string) *http.Request {
			t.Helper()
			body := `{"model":"claude-haiku-4-5","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
			req.Header.Set("Authorization", "Bearer "+rawToken)
			req.Header.Set("Content-Type", "application/json")
			return req
		},
	})
	compareToGolden(t, "passthrough_clean_anthropic", rows)
}

// TestCharacterization_ControlNoticeInjected pins the audit shape when the
// request declares tools and the control plane is configured — the
// `control_notice_injected: true` flag must appear, and the rest of the
// preprocess flags should remain in their declared state.
func TestCharacterization_ControlNoticeInjected(t *testing.T) {
	rows := runCharacterizationScenario(t, characterizationScenario{
		name: "control_notice_injected",
		upstreamHandler: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"model":"claude-sonnet-4","stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":2}}`))
		},
		configure: func(t *testing.T, h *LLMEndpointHandler) {
			h.ControlBaseURL = "http://localhost:25297"
		},
		request: func(t *testing.T, rawToken string) *http.Request {
			t.Helper()
			body := `{"model":"claude-sonnet-4","max_tokens":10,"tools":[{"name":"Bash"}],"messages":[{"role":"user","content":"hi"}]}`
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
			req.Header.Set("Authorization", "Bearer "+rawToken)
			req.Header.Set("Content-Type", "application/json")
			return req
		},
	})
	compareToGolden(t, "control_notice_injected", rows)
}

// TestCharacterization_PassthroughCleanOpenAIChat pins the OpenAI Chat
// Completions baseline. Distinct from Anthropic: conversation_id is minted
// on first turn (the `conversation_id_minted: true` flag must appear) and
// `conversation_id_source` must read "minted".
func TestCharacterization_PassthroughCleanOpenAIChat(t *testing.T) {
	rows := runCharacterizationScenario(t, characterizationScenario{
		name: "passthrough_clean_openai_chat",
		upstreamHandler: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"chatcmpl_1","object":"chat.completion","model":"gpt-4o-mini","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`))
		},
		configure: func(t *testing.T, h *LLMEndpointHandler) {
			// Route OpenAI to the same upstream the seeder configured for
			// Anthropic. The seeded handler only wires AnthropicBaseURL; for
			// OpenAI scenarios we need the OpenAI base too.
			h.Forwarder.Upstream.OpenAIBaseURL = h.Forwarder.Upstream.AnthropicBaseURL
		},
		request: func(t *testing.T, rawToken string) *http.Request {
			t.Helper()
			body := `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}`
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
			req.Header.Set("Authorization", "Bearer "+rawToken)
			req.Header.Set("Content-Type", "application/json")
			return req
		},
	})
	compareToGolden(t, "passthrough_clean_openai_chat", rows)
}

// TestCharacterization_MalformedRequest pins the deny path: a body that
// fails parse should short-circuit before any policy runs, with
// `decision: deny`, `outcome: malformed_request`, and minimal params.
func TestCharacterization_MalformedRequest(t *testing.T) {
	rows := runCharacterizationScenario(t, characterizationScenario{
		name: "malformed_request",
		upstreamHandler: func(w http.ResponseWriter, _ *http.Request) {
			t.Errorf("upstream should not be called for malformed requests")
			w.WriteHeader(http.StatusInternalServerError)
		},
		request: func(t *testing.T, rawToken string) *http.Request {
			t.Helper()
			body := `{not valid json`
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
			req.Header.Set("Authorization", "Bearer "+rawToken)
			req.Header.Set("Content-Type", "application/json")
			return req
		},
	})
	compareToGolden(t, "malformed_request", rows)
}
