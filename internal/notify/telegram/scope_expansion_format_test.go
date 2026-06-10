package telegram

import (
	"strings"
	"testing"

	"github.com/clawvisor/clawvisor/pkg/notify"
)

// TestFormatScopeExpansion_ReplacementShowsWasAndNow guards the
// load-bearing renderer fix: replaced entries must surface both the
// prior `why` and the new `why` so the reviewer sees the actual scope
// change, not just the prior rationale relabeled "Updated".
func TestFormatScopeExpansion_ReplacementShowsWasAndNow(t *testing.T) {
	out := formatScopeExpansionMessage(notify.ScopeExpansionRequest{
		TaskID:    "task-1",
		AgentName: "claude",
		Purpose:   "fetch and process emails",
		ReplacedTools: []notify.ReplacedExpansionTool{
			{
				Prior: notify.ExpansionTool{ToolName: "Bash", Why: "Run one curl to list emails"},
				New:   notify.ExpansionTool{ToolName: "Bash", Why: "List emails AND run a local processing script"},
			},
		},
		ReplacedEgress: []notify.ReplacedExpansionEgress{
			{
				Prior: notify.ExpansionEgress{Host: "api.github.com", Why: "List issues"},
				New:   notify.ExpansionEgress{Host: "api.github.com", Why: "List AND comment on issues"},
			},
		},
		ReplacedCredentials: []notify.ReplacedExpansionCredential{
			{
				Prior: notify.ExpansionCredential{VaultItemID: "github:personal", Why: "List issues"},
				New:   notify.ExpansionCredential{VaultItemID: "github:personal", Why: "List AND comment on issues"},
			},
		},
		Reason: "follow-up call needed after listing",
	})

	mustContain := func(needle, label string) {
		t.Helper()
		if !strings.Contains(out, needle) {
			t.Errorf("missing %s: %q\nrendered:\n%s", label, needle, out)
		}
	}
	// Tool: both whys visible.
	mustContain("was: Run one curl to list emails", "prior tool why")
	mustContain("now: List emails AND run a local processing script", "new tool why")
	// Egress: both whys visible.
	mustContain("was: List issues", "prior egress why")
	mustContain("now: List AND comment on issues", "new egress why")
	// Credential: identifier surfaced from the New entry, both whys visible.
	mustContain("github:personal", "credential id")
}

// TestFormatScopeExpansion_GatewayActionShowsAutoExecuteDisposition is
// the Telegram-side guard for the safety-posture signal: when a derived
// gateway scope would auto-execute, the prompt must say so. Local
// (non-gateway) tools must NOT display the marker.
func TestFormatScopeExpansion_GatewayActionShowsAutoExecuteDisposition(t *testing.T) {
	out := formatScopeExpansionMessage(notify.ScopeExpansionRequest{
		TaskID:    "task-1",
		AgentName: "claude",
		Purpose:   "broaden github access",
		AddedTools: []notify.ExpansionTool{
			{ToolName: "github:create_issue", Why: "Create the follow-up issue", GatewayAction: true, AutoExecute: true},
			{ToolName: "github:delete_repo", Why: "Delete the obsolete repo", GatewayAction: true, AutoExecute: false},
			{ToolName: "Edit", Why: "Local-only fix to processing script"},
		},
		Reason: "follow-up gateway scope and a local edit",
	})

	mustContain := func(needle, label string) {
		t.Helper()
		if !strings.Contains(out, needle) {
			t.Errorf("missing %s: %q\nrendered:\n%s", label, needle, out)
		}
	}
	mustContain("github:create_issue", "auto-execute tool name")
	mustContain("auto-execute", "auto-execute disposition marker")
	mustContain("github:delete_repo", "manual tool name")
	mustContain("per-call approval", "manual disposition marker")
	mustContain("Edit", "local tool name")
	// Local tools must not be tagged with either marker.
	editIdx := strings.Index(out, "Edit")
	if editIdx < 0 {
		t.Fatalf("Edit entry missing from render:\n%s", out)
	}
	// The line containing "Edit" should not also contain the gateway
	// disposition markers. Search until newline.
	editLineEnd := strings.Index(out[editIdx:], "\n")
	if editLineEnd < 0 {
		editLineEnd = len(out) - editIdx
	}
	editLine := out[editIdx : editIdx+editLineEnd]
	if strings.Contains(editLine, "auto-execute") || strings.Contains(editLine, "per-call approval") {
		t.Errorf("local Edit tool rendered with gateway disposition marker: %q", editLine)
	}
}

// TestFormatScopeExpansion_NewEntriesShowOnlyWhy confirms genuinely-new
// (non-replacement) entries render with a single `why` (no was/now) so
// the prompt doesn't lie about a prior version that doesn't exist.
func TestFormatScopeExpansion_NewEntriesShowOnlyWhy(t *testing.T) {
	out := formatScopeExpansionMessage(notify.ScopeExpansionRequest{
		TaskID:    "task-1",
		AgentName: "claude",
		Purpose:   "fetch and process emails",
		AddedTools: []notify.ExpansionTool{
			{ToolName: "Edit", Why: "Apply fixes to the processing script"},
		},
		Reason: "new local tool needed",
	})
	if !strings.Contains(out, "Apply fixes to the processing script") {
		t.Errorf("new tool why missing from render:\n%s", out)
	}
	if strings.Contains(out, "was: ") {
		t.Errorf("new entry rendered with a was/now diff; new entries have no prior:\n%s", out)
	}
}
