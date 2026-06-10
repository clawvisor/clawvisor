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
			name:      "openai_chat_lite_proxy_route",
			provider:  conversation.ProviderOpenAI,
			path:      "/api/v1/chat/completions",
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
			out, ok, err := editor.ReplaceLatestUserText("approve", "", replacement)
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

// TestReplaceLatestUserText_ApprovalIDExpectation covers the three
// shapes the body editor needs to handle:
//
//   - explicit ID in the user message that MATCHES the expected hold —
//     proceed as before.
//   - explicit ID in the user message that DIFFERS from the expected
//     hold — refuse, so a races-between-peek-and-rewrite second hold
//     cannot be released by the prior message.
//   - bare verb (no ID) — fall through to verb-only matching, preserving
//     the documented "approve" / "yes" / "deny" / "no" UX. The expected
//     ID is the *caller's* knowledge of which hold Peek resolved, not
//     a requirement on the user.
//
// The actual-id used in fixtures matches the ID grammar enforced by
// internal/runtime/conversation.approvalReplyRE (cv- followed by 12 or
// 26 alphanumeric chars).
func TestReplaceLatestUserText_ApprovalIDExpectation(t *testing.T) {
	const actualID = "cv-abcdef123456"    // 12-char body
	const differentID = "cv-zzzzzz999999" // also 12-char, different

	cases := []struct {
		name     string
		provider conversation.Provider
		path     string
		body     string
	}{
		{
			name:     "anthropic_blocks_with_id",
			provider: conversation.ProviderAnthropic,
			path:     "/v1/messages",
			body:     `{"messages":[{"role":"user","content":[{"type":"text","text":"approve ` + actualID + `"}]}]}`,
		},
		{
			name:     "openai_responses_string_input",
			provider: conversation.ProviderOpenAI,
			path:     "/v1/responses",
			body:     `{"input":"approve ` + actualID + `"}`,
		},
		{
			name:     "openai_responses_blocks",
			provider: conversation.ProviderOpenAI,
			path:     "/v1/responses",
			body:     `{"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"approve ` + actualID + `"}]}]}`,
		},
		{
			name:     "openai_chat_string_content",
			provider: conversation.ProviderOpenAI,
			path:     "/v1/chat/completions",
			body:     `{"messages":[{"role":"user","content":"approve ` + actualID + `"}]}`,
		},
		{
			name:     "openai_chat_lite_proxy_route",
			provider: conversation.ProviderOpenAI,
			path:     "/api/v1/chat/completions",
			body:     `{"messages":[{"role":"user","content":"approve ` + actualID + `"}]}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name+"/explicit_mismatch_refuses", func(t *testing.T) {
			req := httptest.NewRequest("POST", tc.path, nil)
			editor, ok := newApprovalBodyEditor(req, tc.provider, []byte(tc.body))
			if !ok {
				t.Fatal("expected provider body editor")
			}
			out, rewrote, err := editor.ReplaceLatestUserText("approve", differentID, "[replacement]")
			if err != nil {
				t.Fatalf("ReplaceLatestUserText error: %v", err)
			}
			if rewrote {
				t.Fatalf("expected refusal on ID mismatch, but body was rewritten: %s", out)
			}
		})
		t.Run(tc.name+"/explicit_match_rewrites", func(t *testing.T) {
			req := httptest.NewRequest("POST", tc.path, nil)
			editor, ok := newApprovalBodyEditor(req, tc.provider, []byte(tc.body))
			if !ok {
				t.Fatal("expected provider body editor")
			}
			out, rewrote, err := editor.ReplaceLatestUserText("approve", actualID, "[replacement]")
			if err != nil {
				t.Fatalf("ReplaceLatestUserText error: %v", err)
			}
			if !rewrote {
				t.Fatalf("expected rewrite on matching ID, body unchanged: %s", out)
			}
			if !strings.Contains(string(out), "[replacement]") {
				t.Fatalf("expected replacement in rewritten body: %s", out)
			}
		})
	}

	// Bare-verb replies (the documented common case) must still rewrite
	// even when the caller threads in an expected ID. The parsed ID is
	// empty, so the stricter check is skipped and verb-only matching
	// applies — preserving "approve" / "yes" / "deny" / "no" UX.
	bareCases := []struct {
		name     string
		provider conversation.Provider
		path     string
		body     string
	}{
		{
			name:     "anthropic_string_bare",
			provider: conversation.ProviderAnthropic,
			path:     "/v1/messages",
			body:     `{"messages":[{"role":"user","content":"yes"}]}`,
		},
		{
			name:     "openai_chat_bare",
			provider: conversation.ProviderOpenAI,
			path:     "/v1/chat/completions",
			body:     `{"messages":[{"role":"user","content":"approve"}]}`,
		},
		{
			name:     "openai_chat_lite_proxy_bare",
			provider: conversation.ProviderOpenAI,
			path:     "/api/v1/chat/completions",
			body:     `{"messages":[{"role":"user","content":"approve"}]}`,
		},
		{
			name:     "openai_responses_bare_string",
			provider: conversation.ProviderOpenAI,
			path:     "/v1/responses",
			body:     `{"input":"approve"}`,
		},
	}
	for _, tc := range bareCases {
		t.Run(tc.name+"/bare_verb_with_expected_id_rewrites", func(t *testing.T) {
			req := httptest.NewRequest("POST", tc.path, nil)
			editor, ok := newApprovalBodyEditor(req, tc.provider, []byte(tc.body))
			if !ok {
				t.Fatal("expected provider body editor")
			}
			out, rewrote, err := editor.ReplaceLatestUserText("approve", actualID, "[replacement]")
			if err != nil {
				t.Fatalf("ReplaceLatestUserText error: %v", err)
			}
			if !rewrote {
				t.Fatalf("expected bare-verb rewrite to still succeed when expectation is set; body: %s", out)
			}
			if !strings.Contains(string(out), "[replacement]") {
				t.Fatalf("expected replacement in rewritten body: %s", out)
			}
		})
	}
}

// TestApprovalBodyEditorAskUserQuestionRewriteAnthropic covers the
// AskUserQuestion shape on the Anthropic editor: the user's choice
// arrives as a tool_result block linked to a prior AskUserQuestion
// tool_use whose question text carries the approval marker. The
// editor should detect this as approve+id and rewrite the
// tool_result's content (preserving the tool_use_id) to the notice.
func TestApprovalBodyEditorAskUserQuestionRewriteAnthropic(t *testing.T) {
	const approvalID = "cv-askuq00000099"
	const replacement = "[task created cv-task1]"
	body := `{"messages":[` +
		`{"role":"user","content":"please do the thing"},` +
		`{"role":"assistant","content":[` +
		`{"type":"tool_use","id":"toolu_x","name":"AskUserQuestion","input":{"questions":[{"question":"go ahead? [clawvisor:approval=` + approvalID + `]","options":[{"label":"yes"},{"label":"no"}]}]}}` +
		`]},` +
		`{"role":"user","content":[` +
		`{"type":"tool_result","tool_use_id":"toolu_x","content":"yes"}` +
		`]}` +
		`]}`

	req := httptest.NewRequest("POST", "/v1/messages", nil)
	editor, ok := newApprovalBodyEditor(req, conversation.ProviderAnthropic, []byte(body))
	if !ok {
		t.Fatal("expected anthropic body editor")
	}
	verb, gotID, ok := editor.LatestApprovalReply()
	if !ok || verb != "approve" || gotID != approvalID {
		t.Fatalf("LatestApprovalReply=(%q,%q,%v), want (approve,%q,true)", verb, gotID, ok, approvalID)
	}

	out, rewrote, err := editor.ReplaceLatestUserText("approve", approvalID, replacement)
	if err != nil {
		t.Fatalf("ReplaceLatestUserText error: %v", err)
	}
	if !rewrote {
		t.Fatalf("expected AskUserQuestion-shaped rewrite to succeed, body: %s", out)
	}
	if !json.Valid(out) {
		t.Fatalf("rewritten body not valid JSON: %s", out)
	}
	got := string(out)
	if !strings.Contains(got, replacement) {
		t.Fatalf("rewritten body missing replacement %q: %s", replacement, got)
	}
	// tool_result block must be REPLACED with a text block. The
	// orphan tool_use_id is gone (so historystrip can drop the
	// dangling AskUserQuestion call without Anthropic 400'ing the
	// next request) and the notice survives as a plain text block.
	if strings.Contains(got, `"tool_use_id":"toolu_x"`) {
		t.Fatalf("expected tool_result block to be replaced with text block (dropping tool_use_id), got: %s", got)
	}
	if !strings.Contains(got, `"type":"text"`) {
		t.Fatalf("expected text block in rewritten body, got: %s", got)
	}
	// the original "yes" content should be replaced, not appended
	if strings.Contains(got, `"content":"yes"`) {
		t.Fatalf("rewritten body still carries original tool_result content: %s", got)
	}
}

// TestApprovalBodyEditorAskUserQuestionMismatchedIDRefuses guards the
// cross-conversation spoofing path: a tool_result whose question
// marker doesn't match the expected approval ID must NOT rewrite,
// matching the text path's mismatched-ID refusal semantics.
func TestApprovalBodyEditorAskUserQuestionMismatchedIDRefuses(t *testing.T) {
	const questionID = "cv-askuq00000010"
	const expectedID = "cv-different0000"
	body := `{"messages":[` +
		`{"role":"assistant","content":[` +
		`{"type":"tool_use","id":"toolu_y","name":"AskUserQuestion","input":{"questions":[{"question":"go? [clawvisor:approval=` + questionID + `]","options":[{"label":"yes"},{"label":"no"}]}]}}` +
		`]},` +
		`{"role":"user","content":[` +
		`{"type":"tool_result","tool_use_id":"toolu_y","content":"yes"}` +
		`]}` +
		`]}`
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	editor, ok := newApprovalBodyEditor(req, conversation.ProviderAnthropic, []byte(body))
	if !ok {
		t.Fatal("expected anthropic body editor")
	}
	out, rewrote, err := editor.ReplaceLatestUserText("approve", expectedID, "[notice]")
	if err != nil {
		t.Fatalf("ReplaceLatestUserText error: %v", err)
	}
	if rewrote {
		t.Fatalf("expected rewrite to refuse on ID mismatch; got rewrite: %s", out)
	}
	if strings.Contains(string(out), "[notice]") {
		t.Fatalf("body must remain unchanged on ID mismatch: %s", out)
	}
}

// TestApprovalBodyEditorAskUserQuestionUnrelatedToolResultRefuses
// confirms a tool_result for a non-AskUserQuestion tool_use never
// triggers the approval rewrite, even if the result content happens
// to read "yes". The detection path must verify the parent tool_use's
// name AND the marker before treating the reply as an approval.
func TestApprovalBodyEditorAskUserQuestionUnrelatedToolResultRefuses(t *testing.T) {
	body := `{"messages":[` +
		`{"role":"assistant","content":[` +
		`{"type":"tool_use","id":"toolu_z","name":"Bash","input":{"command":"echo yes"}}` +
		`]},` +
		`{"role":"user","content":[` +
		`{"type":"tool_result","tool_use_id":"toolu_z","content":"yes\n"}` +
		`]}` +
		`]}`
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	editor, ok := newApprovalBodyEditor(req, conversation.ProviderAnthropic, []byte(body))
	if !ok {
		t.Fatal("expected anthropic body editor")
	}
	verb, gotID, replyOK := editor.LatestApprovalReply()
	if replyOK || verb != "" || gotID != "" {
		t.Fatalf("LatestApprovalReply=(%q,%q,%v), want empty (non-AskUserQuestion tool_use)", verb, gotID, replyOK)
	}
}
