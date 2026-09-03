package llmjudge

import (
	"strings"
	"testing"
)

// TestParseJSON exercises the verdict parser directly, including the
// lenient envelope tolerance the validator pattern relies on (leading
// prose, ```json fences, etc.) and the bytewise cap on agent_guidance.
func TestParseJSON(t *testing.T) {
	const longGuidance = "x" + // sentinel char so we can verify the prefix survived
		"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA" + // 100
		"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA" + // 200
		"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA" + // 300
		"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA" + // 400
		"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA" + // 500
		"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA" + // 600
		"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA" + // 700
		"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA" + // 800
		"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA" // 900 (well over the 800-byte cap)

	cases := []struct {
		name         string
		raw          string
		wantAllow    bool
		wantReason   string
		wantGuidance string
		wantErr      bool
	}{
		{
			name:         "allow verdict (guidance should be empty)",
			raw:          `{"verdict":"allow","reason":"variable holds the resolver URL","agent_guidance":""}`,
			wantAllow:    true,
			wantReason:   "variable holds the resolver URL",
			wantGuidance: "",
		},
		{
			name:         "allow verdict ignores any agent_guidance the model returned",
			raw:          `{"verdict":"allow","reason":"ok","agent_guidance":"should be dropped on allow"}`,
			wantAllow:    true,
			wantReason:   "ok",
			wantGuidance: "", // Verdict shape only carries guidance on block
		},
		{
			name:         "block verdict carries guidance verbatim",
			raw:          `{"verdict":"block","reason":"curl targets gmail.googleapis.com directly","agent_guidance":"replace https://gmail.googleapis.com with http://localhost:25297/api/proxy"}`,
			wantAllow:    false,
			wantReason:   "curl targets gmail.googleapis.com directly",
			wantGuidance: "replace https://gmail.googleapis.com with http://localhost:25297/api/proxy",
		},
		{
			name:         "code fence preface",
			raw:          "```json\n{\"verdict\":\"allow\",\"reason\":\"ok\",\"agent_guidance\":\"\"}\n```",
			wantAllow:    true,
			wantReason:   "ok",
			wantGuidance: "",
		},
		{
			name:         "leading prose then JSON",
			raw:          "Here's the JSON:\n{\"verdict\":\"block\",\"reason\":\"no http request\",\"agent_guidance\":\"emit the curl directly\"}",
			wantAllow:    false,
			wantReason:   "no http request",
			wantGuidance: "emit the curl directly",
		},
		{name: "unknown verdict word", raw: `{"verdict":"maybe","reason":"unsure","agent_guidance":""}`, wantErr: true},
		{name: "malformed JSON", raw: `not json`, wantErr: true},
		{
			name:      "over-length agent_guidance gets truncated with marker",
			raw:       `{"verdict":"block","reason":"r","agent_guidance":"` + longGuidance + `"}`,
			wantAllow: false,
			wantReason: "r",
			// Don't pin exact contents; assert truncation properties below.
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseJSON(tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseJSON(%q) succeeded; want error", tc.raw)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseJSON(%q): %v", tc.raw, err)
			}
			if got.Allow != tc.wantAllow {
				t.Errorf("Allow = %v, want %v", got.Allow, tc.wantAllow)
			}
			if got.Reason != tc.wantReason {
				t.Errorf("Reason = %q, want %q", got.Reason, tc.wantReason)
			}
			// Truncation case asserts shape, not exact contents.
			if tc.name == "over-length agent_guidance gets truncated with marker" {
				if len(got.AgentGuidance) > maxAgentGuidanceBytes+len("…(truncated)") {
					t.Errorf("AgentGuidance length %d exceeds cap+marker bound", len(got.AgentGuidance))
				}
				if !strings.HasSuffix(got.AgentGuidance, "…(truncated)") {
					t.Errorf("AgentGuidance should end with truncation marker, got tail %q", got.AgentGuidance[max(0, len(got.AgentGuidance)-30):])
				}
				if !strings.HasPrefix(got.AgentGuidance, "x") {
					t.Errorf("AgentGuidance should preserve leading content (sentinel 'x'), got prefix %q", got.AgentGuidance[:min(10, len(got.AgentGuidance))])
				}
				return
			}
			if got.AgentGuidance != tc.wantGuidance {
				t.Errorf("AgentGuidance = %q, want %q", got.AgentGuidance, tc.wantGuidance)
			}
		})
	}
}

// TestPromptSHA confirms the SHA accessor returns a stable hex
// digest of the configured prompt. Mostly anti-regression: if the
// prompt is edited intentionally, the new hash should surface in
// audit fixtures without requiring a separate test update.
func TestPromptSHA(t *testing.T) {
	j := New(nil, nil)
	got := j.PromptSHA()
	if len(got) != 64 {
		t.Errorf("PromptSHA = %q (len %d), want 64-char hex digest", got, len(got))
	}
	// Stable across calls.
	if again := j.PromptSHA(); again != got {
		t.Errorf("PromptSHA not deterministic: %q vs %q", got, again)
	}
}
