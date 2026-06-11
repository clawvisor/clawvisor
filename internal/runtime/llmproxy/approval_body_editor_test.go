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
			out, ok, err := editor.ReplaceLatestUserText("approve", "", replacement, nil)
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
			out, rewrote, err := editor.ReplaceLatestUserText("approve", differentID, "[replacement]", nil)
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
			out, rewrote, err := editor.ReplaceLatestUserText("approve", actualID, "[replacement]", nil)
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
			out, rewrote, err := editor.ReplaceLatestUserText("approve", actualID, "[replacement]", nil)
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

	out, rewrote, err := editor.ReplaceLatestUserText("approve", approvalID, replacement, nil)
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
	out, rewrote, err := editor.ReplaceLatestUserText("approve", expectedID, "[notice]", nil)
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

// TestApprovalBodyEditorAskUserQuestionReconstructsOriginalCall pins
// the conversation-reconstruction fix: when the body editor is given
// a non-nil OriginalCall snapshot, it MUST replace the substituted-
// prompt assistant turn with a synthetic [tool_use(original)] turn
// AND pair the user-turn tool_result against that reconstructed
// tool_use_id. Without this the model has no record in history of
// having called the substituted endpoint and re-emits the same call
// on the next turn — the user-reported "agent keeps trying to
// expand" failure mode this fix targets.
func TestApprovalBodyEditorAskUserQuestionReconstructsOriginalCall(t *testing.T) {
	const approvalID = "cv-askuq00000020"
	const askToolUseID = "toolu_clawvisor_ask_x"
	const originalToolUseID = "toolu_01OriginalCurlPOST"
	body := `{"messages":[` +
		`{"role":"user","content":"expand the task"},` +
		`{"role":"assistant","content":[` +
		`{"type":"text","text":"Clawvisor wants to expand the scope of an existing task. [clawvisor:approval=` + approvalID + `]"},` +
		`{"type":"tool_use","id":"` + askToolUseID + `","name":"AskUserQuestion","input":{"questions":[{"question":"approve? [clawvisor:approval=` + approvalID + `]","options":[{"label":"yes"},{"label":"no"}]}]}}` +
		`]},` +
		`{"role":"user","content":[` +
		`{"type":"tool_result","tool_use_id":"` + askToolUseID + `","content":"yes"}` +
		`]}` +
		`]}`
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	editor, ok := newApprovalBodyEditor(req, conversation.ProviderAnthropic, []byte(body))
	if !ok {
		t.Fatal("expected anthropic body editor")
	}
	reconstruction := &InlineApprovalOriginalCall{
		ToolUseID: originalToolUseID,
		ToolName:  "Bash",
		Input:     json.RawMessage(`{"command":"curl -X POST /api/control/tasks/X/expand?surface=inline ..."}`),
	}
	out, rewrote, err := editor.ReplaceLatestUserText("approve", approvalID, "[clawvisor-notice] Task scope was expanded and approved.", reconstruction)
	if err != nil {
		t.Fatalf("ReplaceLatestUserText: %v", err)
	}
	if !rewrote {
		t.Fatalf("expected reconstruction rewrite to succeed; body: %s", out)
	}
	got := string(out)

	// The synthetic assistant turn must carry the model's original
	// tool_use_id, not the AskUserQuestion id (which would orphan).
	if !strings.Contains(got, `"id":"`+originalToolUseID+`"`) {
		t.Errorf("reconstructed assistant turn missing original tool_use_id: %s", got)
	}
	if strings.Contains(got, `"id":"`+askToolUseID+`"`) {
		t.Errorf("reconstructed body must not retain the AskUserQuestion tool_use id; got: %s", got)
	}
	// The tool input must round-trip verbatim — the model expects
	// to see exactly what it would have emitted.
	if !strings.Contains(got, "curl -X POST /api/control/tasks/X/expand") {
		t.Errorf("reconstructed tool_input missing original curl command: %s", got)
	}
	// The user-turn tool_result must be PAIRED to the reconstructed
	// tool_use_id, not the AskUserQuestion id. Otherwise Anthropic
	// rejects the next request with the same orphan-tool_use_id 400
	// that the historystrip fix targeted.
	if !strings.Contains(got, `"tool_use_id":"`+originalToolUseID+`"`) {
		t.Errorf("reconstructed tool_result must pair against original tool_use_id: %s", got)
	}
	// The augmentation notice text lands on the tool_result content
	// (the model's "what came back from my call").
	if !strings.Contains(got, "Task scope was expanded and approved") {
		t.Errorf("reconstructed tool_result missing notice text: %s", got)
	}
	if !json.Valid(out) {
		t.Errorf("rewritten body not valid JSON: %s", got)
	}
}

// TestApprovalBodyEditorReconstructionDenySkipped confirms the
// rewrite path does NOT reconstruct on a deny — the model should
// see the denial, not synthetic evidence of a successful call. The
// rewrite caller already gates on verb=="approve" before passing
// reconstruction, but this test pins the body editor's behavior
// regardless: a deny verb with a nil reconstruction falls through
// to the legacy text-block swap.
func TestApprovalBodyEditorReconstructionDenySkipped(t *testing.T) {
	const approvalID = "cv-askuq00000021"
	const askToolUseID = "toolu_clawvisor_ask_y"
	body := `{"messages":[` +
		`{"role":"assistant","content":[` +
		`{"type":"tool_use","id":"` + askToolUseID + `","name":"AskUserQuestion","input":{"questions":[{"question":"approve? [clawvisor:approval=` + approvalID + `]","options":[{"label":"yes"},{"label":"no"}]}]}}` +
		`]},` +
		`{"role":"user","content":[` +
		`{"type":"tool_result","tool_use_id":"` + askToolUseID + `","content":"no"}` +
		`]}` +
		`]}`
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	editor, ok := newApprovalBodyEditor(req, conversation.ProviderAnthropic, []byte(body))
	if !ok {
		t.Fatal("expected anthropic body editor")
	}
	out, rewrote, err := editor.ReplaceLatestUserText("deny", approvalID, "[notice] denied", nil)
	if err != nil {
		t.Fatalf("ReplaceLatestUserText: %v", err)
	}
	if !rewrote {
		t.Fatalf("expected deny rewrite to succeed: %s", out)
	}
	// The legacy text-block swap path leaves the AskUserQuestion
	// tool_use in the assistant turn (historystrip cleans it up
	// later) — so the original tool_use_id MAY still appear. What
	// matters is that nothing NEW was synthesized: the AskUserQuestion
	// tool_use is the only tool_use in the body.
	if strings.Count(string(out), `"type":"tool_use"`) > 1 {
		t.Errorf("deny rewrite must not synthesize an additional tool_use; got: %s", out)
	}
	// The user turn must carry the notice as a text block (not a
	// tool_result paired against some other tool_use_id).
	if !strings.Contains(string(out), `"type":"text"`) {
		t.Errorf("deny rewrite must replace tool_result with text block: %s", out)
	}
	if !strings.Contains(string(out), "[notice] denied") {
		t.Errorf("deny notice missing from rewritten body: %s", out)
	}
}
