package screens

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/clawvisor/clawvisor/internal/tui/client"
)

// TestWritePendingExpansionSummary_RendersDiff exercises the bullet-list
// renderer on a representative pending expansion: a NEW tool, a
// REPLACED tool (with was/now diff on `why`), a NEW egress, a NEW
// credential, and a REPLACED credential. The trimmed-vs-untrimmed
// colon guard on autoExecuteMarker is exercised separately below;
// here we want to catch the "renders blank section" / "wrong + vs ~
// marker" regressions that would otherwise ship silently — there
// were zero tests in internal/tui/ before this file.
func TestWritePendingExpansionSummary_RendersDiff(t *testing.T) {
	task := &client.Task{
		ExpectedTools: []client.ExpectedTool{
			{ToolName: "Bash", Why: "Run a single curl to list emails"},
		},
		RequiredCredentials: []client.RequiredCredential{
			{VaultItemID: "github:foo", Why: "List issues"},
		},
		PendingExpansion: &client.PendingTaskExpansion{
			ExpectedTools: mustMarshalT(t, []map[string]any{
				{"tool_name": "Edit", "why": "Apply fixes to processing script"},
				{"tool_name": "Bash", "why": "List emails AND run processing script"},
			}),
			ExpectedEgress: mustMarshalT(t, []map[string]any{
				{"host": "api.openai.com", "why": "Summarize the email text"},
			}),
			RequiredCredentials: mustMarshalT(t, []map[string]any{
				{"vault_item_id": "github:foo", "why": "Comment on issues"},
				{"vault_item_handle": "openai:personal", "why": "Authorize the new LLM call"},
			}),
			Reason: "downstream summary step",
		},
	}

	var b strings.Builder
	writePendingExpansionSummary(&b, task)
	out := b.String()

	wantContains := []string{
		"+ Edit",                       // genuinely new tool
		"Apply fixes to processing",    // new tool's why
		"~ Bash",                       // replaced tool — was/now diff
		"was: Run a single curl",       // prior why surfaced for tool
		"now: List emails AND run",     // new why surfaced for tool
		"+ api.openai.com",             // new egress
		"Summarize the email text",     // new egress's why
		"~ github:foo",                 // credential replaced
		"was: List issues",             // prior credential why
		"now: Comment on issues",       // new credential why
		"+ openai:personal",            // new credential by handle
		"Authorize the new LLM call",   // new credential's why
		"Reason: downstream summary",   // top-level reason line
	}
	for _, want := range wantContains {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n---\n%s", want, out)
		}
	}
	// Spot-check the inverse: the parent's prior `why` for github:foo
	// should NOT appear next to the credential's new entry without
	// the was/now markers — otherwise the diff would silently collapse.
	if strings.Count(out, "was: List issues") != 1 {
		t.Errorf("expected exactly one 'was: List issues' line, got: %s", out)
	}
}

// TestAutoExecuteMarker_TrailingColonGuard locks in the colon-guard
// fix the inline comment describes: an earlier version compared idx
// (from trimmed) against len(toolName) (untrimmed), which let
// "github:" with trailing whitespace slip past the guard and look up
// a malformed key. Without this test, the off-by-N would silently
// regress.
func TestAutoExecuteMarker_TrailingColonGuard(t *testing.T) {
	derived := map[string]client.TaskAction{
		"github:create_issue": {Service: "github", Action: "create_issue", AutoExecute: false},
	}
	cases := []struct {
		name string
		tool string
		want bool // any marker rendered
	}{
		{"bare tool (no colon)", "Bash", false},
		{"trailing colon, untrimmed", "github:\t", false},
		{"leading colon", ":action", false},
		{"valid match", "github:create_issue", true},
		// Case-insensitive lookup so a mismatched-case tool name still
		// finds its derived action — mirrors the merge dedup key.
		{"case-insensitive match", "GitHub:Create_Issue", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := autoExecuteMarker(tc.tool, derived, nil)
			has := got != ""
			if has != tc.want {
				t.Errorf("autoExecuteMarker(%q) = %q (rendered=%v), want rendered=%v", tc.tool, got, has, tc.want)
			}
		})
	}
}

// TestAutoExecuteMarker_WildcardCoverage exercises the wildcard
// fallback: an addition whose specific derivation was dropped by
// mergeAuthorizedActionsFromExpansion (because parent has a
// same-service wildcard) still renders a marker derived from the
// wildcard's AutoExecute. Without this branch, the TUI would show
// "needs per-call approval" on an action the user already
// auto-approved through the wildcard.
func TestAutoExecuteMarker_WildcardCoverage(t *testing.T) {
	derived := map[string]client.TaskAction{}
	wildcards := map[string]client.TaskAction{
		"github": {Service: "github", Action: "*", AutoExecute: true},
		"slack":  {Service: "slack", Action: "*", AutoExecute: false},
	}
	t.Run("auto-execute wildcard", func(t *testing.T) {
		got := autoExecuteMarker("github:create_issue", derived, wildcards)
		if !strings.Contains(got, "covered by wildcard") || !strings.Contains(got, "auto-execute") {
			t.Errorf("got %q, want a wildcard auto-execute marker", got)
		}
	})
	t.Run("per-call wildcard", func(t *testing.T) {
		got := autoExecuteMarker("slack:post_message", derived, wildcards)
		if !strings.Contains(got, "covered by wildcard") || !strings.Contains(got, "per-call") {
			t.Errorf("got %q, want a wildcard per-call marker", got)
		}
	})
	t.Run("derived wins over wildcard", func(t *testing.T) {
		// When both exist, derived takes precedence (the specific
		// entry is what would actually persist; the wildcard fallback
		// is for the dropped-derivation case).
		mixedDerived := map[string]client.TaskAction{
			"github:create_issue": {Service: "github", Action: "create_issue", AutoExecute: false},
		}
		got := autoExecuteMarker("github:create_issue", mixedDerived, wildcards)
		if strings.Contains(got, "covered by wildcard") {
			t.Errorf("got %q, want the specific derived marker (no wildcard label)", got)
		}
	})
}

func mustMarshalT(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}
