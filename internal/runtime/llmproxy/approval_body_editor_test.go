package llmproxy

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
)

func TestApprovalBodyEditorProviderShapes(t *testing.T) {
	const replacement = "[replacement context]"

	cases := []struct {
		name       string
		provider   conversation.Provider
		path       string
		body       string
		wantReply  bool
		wantVerb   string
		wantID     string
		want       string
		wantAbsent string
	}{
		{
			name:      "anthropic_string_content",
			provider:  conversation.ProviderAnthropic,
			path:      "/v1/messages",
			body:      `{"messages":[{"role":"user","content":"yes"}]}`,
			wantReply: true,
			wantVerb:  "approve",
			want:      `"content":"` + replacement + `"`,
		},
		{
			name:     "anthropic_text_blocks",
			provider: conversation.ProviderAnthropic,
			path:     "/v1/messages",
			body: `{"messages":[{"role":"user","content":[` +
				`{"type":"text","text":"approve cv-abcdef123456"},` +
				`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"abc"}}]}]}`,
			wantReply:  true,
			wantVerb:   "approve",
			wantID:     "cv-abcdef123456",
			want:       `"content":"` + replacement + `"`,
			wantAbsent: "cv-abcdef123456",
		},
		{
			name:      "openai_chat_string_content",
			provider:  conversation.ProviderOpenAI,
			path:      "/v1/chat/completions",
			body:      `{"messages":[{"role":"user","content":"approve"}]}`,
			wantReply: true,
			wantVerb:  "approve",
			want:      `"content":"` + replacement + `"`,
		},
		{
			name:      "openai_responses_string_input",
			provider:  conversation.ProviderOpenAI,
			path:      "/v1/responses",
			body:      `{"input":"approve"}`,
			wantReply: true,
			wantVerb:  "approve",
			want:      `"input":"` + replacement + `"`,
		},
		{
			name:     "openai_responses_message_blocks",
			provider: conversation.ProviderOpenAI,
			path:     "/v1/responses",
			body: `{"input":[{"type":"message","role":"user","content":[` +
				`{"type":"input_text","text":"approve cv-abcdef123456"}]}]}`,
			wantReply:  true,
			wantVerb:   "approve",
			wantID:     "cv-abcdef123456",
			want:       `"text":"` + replacement + `"`,
			wantAbsent: "cv-abcdef123456",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", tc.path, nil)
			editor, ok := newApprovalBodyEditor(req, tc.provider, []byte(tc.body))
			if !ok {
				t.Fatal("expected provider body editor")
			}
			verb, approvalID, ok := editor.LatestApprovalReply()
			if ok != tc.wantReply || verb != tc.wantVerb || approvalID != tc.wantID {
				t.Fatalf("LatestApprovalReply=(%q,%q,%v), want (%q,%q,%v)", verb, approvalID, ok, tc.wantVerb, tc.wantID, tc.wantReply)
			}
			out, ok, err := editor.ReplaceLatestUserText("approve", replacement)
			if err != nil {
				t.Fatal(err)
			}
			if !ok {
				t.Fatalf("expected body replacement to succeed")
			}
			if !json.Valid(out) {
				t.Fatalf("replacement produced invalid JSON: %s", out)
			}
			got := string(out)
			if !strings.Contains(got, tc.want) {
				t.Fatalf("rewritten body missing %q: %s", tc.want, got)
			}
			if tc.wantAbsent != "" && strings.Contains(got, tc.wantAbsent) {
				t.Fatalf("rewritten body should not retain %q: %s", tc.wantAbsent, got)
			}
		})
	}
}
