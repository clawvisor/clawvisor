package llmproxy

import (
	"strings"
	"testing"

	runtimetasks "github.com/clawvisor/clawvisor/internal/runtime/tasks"
	"github.com/clawvisor/clawvisor/internal/taskrisk"
)

func TestRenderTaskApprovalPromptWellFormed(t *testing.T) {
	prompt := renderTaskApprovalPrompt(&runtimetasks.TaskCreateRequest{
		Purpose: "Build a landing page at /tmp/landing",
		ExpectedTools: []runtimetasks.ExpectedTool{
			{ToolName: "Bash", Why: "Create directory and helper scripts"},
			{ToolName: "Write", Why: "Create HTML/CSS files"},
		},
		IntentVerificationMode: "strict",
		Lifetime:               "session",
		ExpiresInSeconds:       600,
	}, "")
	if !strings.Contains(prompt, "Clawvisor wants to create a task") {
		t.Errorf("missing header in prompt: %q", prompt)
	}
	if !strings.Contains(prompt, "Build a landing page at /tmp/landing") {
		t.Errorf("missing purpose: %q", prompt)
	}
	if !strings.Contains(prompt, "• Bash") || !strings.Contains(prompt, "• Write") {
		t.Errorf("missing tool bullets: %q", prompt)
	}
	if !strings.Contains(prompt, "Create directory") {
		t.Errorf("missing why text: %q", prompt)
	}
	if strings.Contains(prompt, "Verification:") {
		t.Errorf("strict verification should be hidden as the default: %q", prompt)
	}
	if strings.Contains(prompt, "Lifetime:") {
		t.Errorf("session lifetime should not surface a Lifetime line: %q", prompt)
	}
	if !strings.Contains(prompt, "Duration: 10 min") {
		t.Errorf("missing combined Duration line: %q", prompt)
	}
	if !strings.Contains(prompt, "`yes` or `y`") || !strings.Contains(prompt, "`no` or `n`") {
		t.Errorf("missing yes/no instructions: %q", prompt)
	}
	if strings.Contains(prompt, "{") {
		t.Errorf("raw JSON leaked into prompt: %q", prompt)
	}
}

func TestRenderTaskApprovalPromptStandingLifetime(t *testing.T) {
	prompt := renderTaskApprovalPrompt(&runtimetasks.TaskCreateRequest{
		Purpose:  "Long-running data ingest",
		Lifetime: "standing",
	}, "")
	if !strings.Contains(prompt, "Lifetime: always") {
		t.Errorf("expected 'Lifetime: always' in standing prompt, got %q", prompt)
	}
}

func TestRenderTaskApprovalPromptHidesStrictVerification(t *testing.T) {
	prompt := renderTaskApprovalPrompt(&runtimetasks.TaskCreateRequest{
		Purpose: "Send a single test email",
	}, "")
	if strings.Contains(prompt, "Verification:") {
		t.Errorf("strict (default) verification should be omitted, got %q", prompt)
	}
}

func TestRenderTaskApprovalPromptShowsNonStrictVerification(t *testing.T) {
	prompt := renderTaskApprovalPrompt(&runtimetasks.TaskCreateRequest{
		Purpose:                "x",
		IntentVerificationMode: "lenient",
	}, "")
	if !strings.Contains(prompt, "Verification: lenient") {
		t.Errorf("expected lenient verification surfaced, got %q", prompt)
	}
}

func TestRenderTaskApprovalPromptDefaultsDurationWhenExpiryUnset(t *testing.T) {
	prompt := renderTaskApprovalPrompt(&runtimetasks.TaskCreateRequest{
		Purpose:          "x",
		ExpiresInSeconds: 0,
	}, "")
	if !strings.Contains(prompt, "Duration: 30 min") {
		t.Errorf("expected default 30 min duration when seconds<=0, got %q", prompt)
	}
	if strings.Contains(prompt, "Expires:") {
		t.Errorf("legacy Expires label leaked, got %q", prompt)
	}
}

func TestRenderTaskApprovalPromptFallbackForEmptyPurpose(t *testing.T) {
	prompt := renderTaskApprovalPrompt(&runtimetasks.TaskCreateRequest{
		Purpose: "   ",
	}, "")
	if !strings.Contains(prompt, "unnamed") {
		t.Errorf("expected 'unnamed' fallback, got %q", prompt)
	}
	if !strings.Contains(prompt, "`yes`") {
		t.Errorf("expected yes instructions in fallback, got %q", prompt)
	}
}

func TestRenderTaskApprovalPromptFallbackForNil(t *testing.T) {
	prompt := renderTaskApprovalPrompt(nil, "")
	if !strings.Contains(prompt, "Clawvisor wants to create a task") {
		t.Errorf("nil input: missing fallback text: %q", prompt)
	}
	if strings.Contains(prompt, "{") {
		t.Errorf("nil input: raw JSON leaked: %q", prompt)
	}
}

func TestRenderTaskApprovalPromptWrapsLongPurpose(t *testing.T) {
	longPurpose := "Build and deploy a complete production-ready landing page that demonstrates Clawvisor's inline task approval flow including all the various edge cases people care about"
	prompt := renderTaskApprovalPrompt(&runtimetasks.TaskCreateRequest{
		Purpose: longPurpose,
	}, "")
	// every line should be ≤ 80 cols
	for _, line := range strings.Split(prompt, "\n") {
		if len(line) > 80 {
			t.Errorf("line exceeded 80 cols: %q (len=%d)", line, len(line))
		}
	}
}

func TestRenderTaskApprovalPromptRendersEgress(t *testing.T) {
	prompt := renderTaskApprovalPrompt(&runtimetasks.TaskCreateRequest{
		Purpose: "Fetch GitHub stars",
		ExpectedEgress: []runtimetasks.ExpectedEgress{
			{Host: "api.github.com", Why: "Read public repo metadata"},
		},
	}, "")
	if !strings.Contains(prompt, "Network egress") {
		t.Errorf("expected Network egress section, got %q", prompt)
	}
	if !strings.Contains(prompt, "api.github.com") {
		t.Errorf("expected egress host in prompt, got %q", prompt)
	}
}

func TestRenderTaskApprovalPromptRendersCredentials(t *testing.T) {
	prompt := renderTaskApprovalPrompt(&runtimetasks.TaskCreateRequest{
		Purpose: "Create GitHub release issues",
		ExpectedTools: []runtimetasks.ExpectedTool{
			{ToolName: "Bash", Why: "Call the GitHub API."},
		},
		RequiredCredentials: []runtimetasks.RequiredCredential{
			{VaultItemID: "github", Why: "Create issues in owner/repo."},
		},
	}, "")
	if !strings.Contains(prompt, "Credentials requested") {
		t.Errorf("expected credential section, got %q", prompt)
	}
	if !strings.Contains(prompt, "github") {
		t.Errorf("expected credential id in prompt, got %q", prompt)
	}
	if strings.Contains(prompt, "{") {
		t.Errorf("raw JSON leaked into prompt: %q", prompt)
	}
}

func TestRenderTaskApprovalPromptRendersRisk(t *testing.T) {
	prompt := renderTaskApprovalPromptWithRisk(&runtimetasks.TaskCreateRequest{
		Purpose: "Create GitHub release issues",
		RequiredCredentials: []runtimetasks.RequiredCredential{
			{VaultItemID: "github", Why: "Create issues in owner/repo."},
		},
	}, "", &taskrisk.RiskAssessment{
		RiskLevel:   "medium",
		Explanation: "This task requests credential access.",
	}, 0)
	if !strings.Contains(prompt, "Risk") {
		t.Errorf("expected risk section, got %q", prompt)
	}
	if !strings.Contains(prompt, "🟡 medium") {
		t.Errorf("expected risk level with traffic-light emoji prefix, got %q", prompt)
	}
	if !strings.Contains(prompt, "This task requests credential access.") {
		t.Errorf("expected risk explanation in prompt, got %q", prompt)
	}
}

func TestHumanizeExpiresIn(t *testing.T) {
	cases := map[int]string{
		0:    "",
		-1:   "",
		60:   "1 min",
		120:  "2 min",
		600:  "10 min",
		3600: "1 hour",
		7200: "2 hours",
		45:   "45 sec",
	}
	for input, want := range cases {
		got := humanizeExpiresIn(input)
		if got != want {
			t.Errorf("humanizeExpiresIn(%d) = %q, want %q", input, got, want)
		}
	}
}

func TestDurationLine(t *testing.T) {
	cases := []struct {
		name           string
		lifetime       string
		seconds        int
		defaultSeconds int
		wantLabel      string
		wantValue      string
	}{
		{"session explicit overrides default", "session", 600, 1800, "Duration", "10 min"},
		{"session default from config", "session", 0, 3600, "Duration", "1 hour"},
		{"session falls back to const when no default", "session", 0, 0, "Duration", "30 min"},
		{"empty lifetime + zero default uses const", "", 0, 0, "Duration", "30 min"},
		{"empty lifetime + config default", "", 0, 7200, "Duration", "2 hours"},
		{"empty lifetime + explicit duration", "", 3600, 0, "Duration", "1 hour"},
		{"standing ignores seconds and default", "standing", 0, 3600, "Lifetime", "always"},
		{"standing ignores nonzero seconds", "standing", 600, 1800, "Lifetime", "always"},
		{"unknown lifetime omits line", "weird", 0, 0, "", ""},
		{"negative default treated as unset", "session", 0, -1, "Duration", "30 min"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotLabel, gotValue := durationLine(c.lifetime, c.seconds, c.defaultSeconds)
			if gotLabel != c.wantLabel || gotValue != c.wantValue {
				t.Errorf("durationLine(%q, %d, %d) = (%q, %q), want (%q, %q)",
					c.lifetime, c.seconds, c.defaultSeconds, gotLabel, gotValue, c.wantLabel, c.wantValue)
			}
		})
	}
}

func TestRiskEmoji(t *testing.T) {
	cases := map[string]string{
		"low":      "🟢",
		"medium":   "🟡",
		"high":     "🟠",
		"critical": "🔴",
		"unknown":  "⚪",
		"":         "",
		"weird":    "",
		"LOW":      "🟢",
	}
	for input, want := range cases {
		got := riskEmoji(input)
		if got != want {
			t.Errorf("riskEmoji(%q) = %q, want %q", input, got, want)
		}
	}
}
