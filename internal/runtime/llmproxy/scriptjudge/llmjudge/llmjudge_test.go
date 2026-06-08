package llmjudge

import "testing"

// TestParseJSON exercises the verdict parser directly, including the
// lenient envelope tolerance the validator pattern relies on (leading
// prose, ```json fences, etc.).
func TestParseJSON(t *testing.T) {
	cases := []struct {
		name       string
		raw        string
		wantAllow  bool
		wantReason string
		wantErr    bool
	}{
		{
			name:       "allow verdict",
			raw:        `{"verdict":"allow","reason":"variable holds the resolver URL","agent_guidance":""}`,
			wantAllow:  true,
			wantReason: "variable holds the resolver URL",
		},
		{
			name:       "block verdict with guidance",
			raw:        `{"verdict":"block","reason":"curl targets gmail.googleapis.com directly","agent_guidance":"replace https://gmail.googleapis.com with http://localhost:25297/api/proxy"}`,
			wantAllow:  false,
			wantReason: "curl targets gmail.googleapis.com directly",
		},
		{
			name:       "code fence preface",
			raw:        "```json\n{\"verdict\":\"allow\",\"reason\":\"ok\",\"agent_guidance\":\"\"}\n```",
			wantAllow:  true,
			wantReason: "ok",
		},
		{
			name:       "leading prose then JSON",
			raw:        "Here's the JSON:\n{\"verdict\":\"block\",\"reason\":\"no http request\",\"agent_guidance\":\"emit the curl directly\"}",
			wantAllow:  false,
			wantReason: "no http request",
		},
		{name: "unknown verdict word", raw: `{"verdict":"maybe","reason":"unsure","agent_guidance":""}`, wantErr: true},
		{name: "malformed JSON", raw: `not json`, wantErr: true},
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
