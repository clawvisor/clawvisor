package llmproxy

import (
	"strings"
	"testing"

	runtimetasks "github.com/clawvisor/clawvisor/internal/runtime/tasks"
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
	})
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
	if !strings.Contains(prompt, "Verification: strict") {
		t.Errorf("missing verification line: %q", prompt)
	}
	if !strings.Contains(prompt, "until session ends") {
		t.Errorf("missing humanized lifetime: %q", prompt)
	}
	if !strings.Contains(prompt, "10 min") {
		t.Errorf("missing humanized expiry: %q", prompt)
	}
	if !strings.Contains(prompt, "approve") || !strings.Contains(prompt, "deny") {
		t.Errorf("missing approve/deny instructions: %q", prompt)
	}
	if strings.Contains(prompt, "{") {
		t.Errorf("raw JSON leaked into prompt: %q", prompt)
	}
}

func TestRenderTaskApprovalPromptStandingLifetime(t *testing.T) {
	prompt := renderTaskApprovalPrompt(&runtimetasks.TaskCreateRequest{
		Purpose:  "Long-running data ingest",
		Lifetime: "standing",
	})
	if !strings.Contains(prompt, "Lifetime: always") {
		t.Errorf("expected 'Lifetime: always' in standing prompt, got %q", prompt)
	}
}

func TestRenderTaskApprovalPromptDefaultsVerification(t *testing.T) {
	prompt := renderTaskApprovalPrompt(&runtimetasks.TaskCreateRequest{
		Purpose: "Send a single test email",
	})
	if !strings.Contains(prompt, "Verification: strict") {
		t.Errorf("expected default strict verification, got %q", prompt)
	}
}

func TestRenderTaskApprovalPromptOmitsExpiryWhenUnset(t *testing.T) {
	prompt := renderTaskApprovalPrompt(&runtimetasks.TaskCreateRequest{
		Purpose:                "x",
		ExpiresInSeconds:       0,
		IntentVerificationMode: "lenient",
	})
	if strings.Contains(prompt, "Expires:") {
		t.Errorf("expected no Expires line when seconds<=0, got %q", prompt)
	}
}

func TestRenderTaskApprovalPromptFallbackForEmptyPurpose(t *testing.T) {
	prompt := renderTaskApprovalPrompt(&runtimetasks.TaskCreateRequest{
		Purpose: "   ",
	})
	if !strings.Contains(prompt, "unnamed") {
		t.Errorf("expected 'unnamed' fallback, got %q", prompt)
	}
	if !strings.Contains(prompt, "approve") {
		t.Errorf("expected approve instructions in fallback, got %q", prompt)
	}
}

func TestRenderTaskApprovalPromptFallbackForNil(t *testing.T) {
	prompt := renderTaskApprovalPrompt(nil)
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
	})
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
	})
	if !strings.Contains(prompt, "Network egress") {
		t.Errorf("expected Network egress section, got %q", prompt)
	}
	if !strings.Contains(prompt, "api.github.com") {
		t.Errorf("expected egress host in prompt, got %q", prompt)
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

func TestHumanizeLifetime(t *testing.T) {
	cases := map[string]string{
		"":         "until session ends",
		"session":  "until session ends",
		"standing": "always",
		"weird":    "weird",
	}
	for input, want := range cases {
		got := humanizeLifetime(input)
		if got != want {
			t.Errorf("humanizeLifetime(%q) = %q, want %q", input, got, want)
		}
	}
}
