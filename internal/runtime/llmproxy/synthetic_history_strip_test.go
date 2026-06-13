package llmproxy

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/historystrip"
)

func TestStripSyntheticApprovalHistory_DropsInlinePromptAndBareReply(t *testing.T) {
	body := anthropicTextBody(
		map[string]string{"role": "user", "content": "Can you delete it?"},
		map[string]string{"role": "assistant", "content": promptWithFooter("cv-approve-1", "Delete /tmp/hello.py")},
		map[string]string{"role": "user", "content": "y"},
	)

	out, err := StripSyntheticApprovalHistory(SyntheticApprovalHistoryStripRequest{
		Provider: conversation.ProviderAnthropic,
		Body:     body,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Modified {
		t.Fatal("expected synthetic inline prompt history to be stripped")
	}
	text := string(out.Body)
	if strings.Contains(text, InlineApprovalSubstitutedPromptMarker) || strings.Contains(text, "cv-approve-1") {
		t.Fatalf("approval prompt leaked upstream: %s", text)
	}
	var decoded struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out.Body, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Messages) != 1 {
		t.Fatalf("expected only the real user request to remain; got %+v", decoded.Messages)
	}
	if got := flattenAnthropicTaskReplyText(decoded.Messages[0].Content); got != "Can you delete it?" {
		t.Fatalf("unexpected remaining message: %q", got)
	}
}

func TestStripSyntheticApprovalHistory_DropsAskUserQuestionToolResultOrphan(t *testing.T) {
	// New AskUserQuestion substitution shape: the proxy emits a
	// text block (with marker) + tool_use(AskUserQuestion) in the
	// assistant turn. The harness sends back a tool_result for the
	// AskUserQuestion call in the next user turn. When the strip
	// removes the assistant turn, the tool_result is orphaned and
	// Anthropic returns 400 — so the strip must also drop the
	// matching tool_result blocks from the next user turn.
	const approvalID = "cv-askuq-strip-1"
	body, err := json.Marshal(map[string]any{
		"model": "claude-haiku-4-5",
		"messages": []map[string]any{
			{"role": "user", "content": "Can you create a haiku file?"},
			{"role": "assistant", "content": []map[string]any{
				{"type": "text", "text": "Clawvisor wants to create a task to cover this work:\n\nPurpose\n  Create haiku\n\n[clawvisor:approval=" + approvalID + "]"},
				{"type": "tool_use", "id": "toolu_clawvisor_ask_" + approvalID, "name": "AskUserQuestion", "input": map[string]any{
					"questions": []map[string]any{{"question": "Approve this task?", "options": []map[string]any{{"label": "yes"}, {"label": "no"}}}},
				}},
			}},
			{"role": "user", "content": []map[string]any{
				{"type": "tool_result", "tool_use_id": "toolu_clawvisor_ask_" + approvalID, "content": "Your questions have been answered: \"Approve this task?\"=\"yes\". You can now continue with these answers in mind."},
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := StripSyntheticApprovalHistory(SyntheticApprovalHistoryStripRequest{
		Provider: conversation.ProviderAnthropic,
		Body:     body,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Modified {
		t.Fatal("expected strip to modify body (assistant turn + orphan tool_result)")
	}
	text := string(out.Body)
	if strings.Contains(text, "toolu_clawvisor_ask_"+approvalID) {
		t.Fatalf("orphan AskUserQuestion tool_use_id still present in stripped body: %s", text)
	}
	if strings.Contains(text, "tool_result") {
		t.Fatalf("orphan tool_result block still present in stripped body: %s", text)
	}
	var decoded struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out.Body, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Messages) != 1 {
		t.Fatalf("expected only the original user query to remain; got %+v", decoded.Messages)
	}
}

// TestStripSyntheticApprovalHistory_DropsExpansionApprovalPromptAndToolResultOrphan
// pins the fix for a 400 "tool use concurrency issues" that fired when
// the user approved a scope-expansion via the AskUserQuestion picker:
// the body editor swapped the user-turn tool_result for a text block,
// but the historystrip's substituted-prompt detector only recognized
// the task-creation marker, so the assistant turn (and its
// AskUserQuestion tool_use) stayed in history with no matching
// tool_result. The strip must recognize the expansion-prompt marker
// and remove both ends of the pair.
func TestStripSyntheticApprovalHistory_DropsExpansionApprovalPromptAndToolResultOrphan(t *testing.T) {
	const approvalID = "cv-askuq-expand-1"
	body, err := json.Marshal(map[string]any{
		"model": "claude-haiku-4-5",
		"messages": []map[string]any{
			{"role": "user", "content": "Also reply to the comment."},
			{"role": "assistant", "content": []map[string]any{
				{"type": "text", "text": "Clawvisor wants to expand the scope of an existing task:\n\nTask\n  Investigate alerts\n\nAdditional tools\n  • curl\n\n[clawvisor:approval=" + approvalID + "]"},
				{"type": "tool_use", "id": "toolu_clawvisor_ask_" + approvalID, "name": "AskUserQuestion", "input": map[string]any{
					"questions": []map[string]any{{"question": "Approve this scope expansion?", "options": []map[string]any{{"label": "yes"}, {"label": "no"}}}},
				}},
			}},
			{"role": "user", "content": []map[string]any{
				{"type": "tool_result", "tool_use_id": "toolu_clawvisor_ask_" + approvalID, "content": "Your questions have been answered: \"Approve this scope expansion?\"=\"yes\". You can now continue with these answers in mind."},
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := StripSyntheticApprovalHistory(SyntheticApprovalHistoryStripRequest{
		Provider: conversation.ProviderAnthropic,
		Body:     body,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Modified {
		t.Fatal("expected expansion approval prompt + orphan tool_result to be stripped")
	}
	text := string(out.Body)
	if strings.Contains(text, "toolu_clawvisor_ask_"+approvalID) {
		t.Fatalf("orphan AskUserQuestion tool_use_id still present after strip: %s", text)
	}
	if strings.Contains(text, "tool_result") {
		t.Fatalf("orphan tool_result still present after strip: %s", text)
	}
	if strings.Contains(text, InlineExpansionApprovalSubstitutedPromptMarker) {
		t.Fatalf("expansion approval prompt leaked upstream: %s", text)
	}
}

// TestStripSyntheticApprovalHistory_PreservesReconstructedTurns
// pins that the historystrip leaves the body-editor's reconstructed
// [tool_use, tool_result] pair alone on subsequent turns. The body
// editor replaces the substituted-prompt assistant turn with a
// synthetic [tool_use(original)] turn (no Clawvisor markers) and
// pairs a tool_result against the reconstructed tool_use_id. On the
// next request the strip MUST NOT remove either: the model needs
// the evidence of its own call to avoid re-emitting.
func TestStripSyntheticApprovalHistory_PreservesReconstructedTurns(t *testing.T) {
	// Conversation shape after the body editor reconstructed:
	//   - user (original ask)
	//   - assistant [tool_use(original Bash POST)]
	//   - user [tool_result(original_id, "scope was expanded notice")]
	//   - assistant text (model's next move)
	body, err := json.Marshal(map[string]any{
		"model": "claude-haiku-4-5",
		"messages": []map[string]any{
			{"role": "user", "content": "expand the task"},
			{"role": "assistant", "content": []map[string]any{
				{"type": "tool_use", "id": "toolu_01OriginalCurl", "name": "Bash", "input": map[string]any{
					"command": "curl -X POST .../api/control/tasks/X/expand?surface=inline ...",
				}},
			}},
			{"role": "user", "content": []map[string]any{
				{"type": "tool_result", "tool_use_id": "toolu_01OriginalCurl", "content": "Task scope was expanded and approved."},
			}},
			{"role": "assistant", "content": "Acknowledged. Proceeding."},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := StripSyntheticApprovalHistory(SyntheticApprovalHistoryStripRequest{
		Provider: conversation.ProviderAnthropic,
		Body:     body,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Modified {
		t.Fatalf("strip must leave reconstructed pair alone; got modified body: %s", out.Body)
	}
	got := string(out.Body)
	if !strings.Contains(got, "toolu_01OriginalCurl") {
		t.Errorf("reconstructed tool_use_id should survive strip: %s", got)
	}
	if !strings.Contains(got, "Task scope was expanded and approved") {
		t.Errorf("reconstructed tool_result content should survive strip: %s", got)
	}
}

// TestStripSyntheticApprovalHistory_ReconstructsViaLookup pins
// the persistent-reconstruction contract: on every turn after the
// approval, the strip path REPLACES the substituted-prompt assistant
// turn with a synthetic [tool_use(original)] and pairs the user-turn
// tool_result to that reconstructed id. Without this the model's
// evidence of having called /expand is one-shot (visible only on
// the first post-approval turn) and turns N+2 onwards lose it.
func TestStripSyntheticApprovalHistory_ReconstructsViaLookup(t *testing.T) {
	const approvalID = "cv-persistreconst1"
	const askToolUseID = "toolu_clawvisor_ask_" + approvalID
	const originalToolUseID = "toolu_01OriginalReconstruct"
	body, err := json.Marshal(map[string]any{
		"model": "claude-haiku-4-5",
		"messages": []map[string]any{
			{"role": "user", "content": "expand the task"},
			{"role": "assistant", "content": []map[string]any{
				{"type": "text", "text": "Clawvisor wants to expand the scope of an existing task:\n[clawvisor:approval=" + approvalID + "]"},
				{"type": "tool_use", "id": askToolUseID, "name": "AskUserQuestion", "input": map[string]any{
					"questions": []map[string]any{{"question": "approve?"}},
				}},
			}},
			{"role": "user", "content": []map[string]any{
				{"type": "tool_result", "tool_use_id": askToolUseID, "content": "yes"},
			}},
			{"role": "assistant", "content": "Got it, proceeding."},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	lookup := func(id string) *historystrip.ReconstructedPair {
		if id != approvalID {
			return nil
		}
		return &historystrip.ReconstructedPair{
			ToolUseID:  originalToolUseID,
			ToolName:   "Bash",
			Input:      json.RawMessage(`{"command":"curl -X POST .../expand?surface=inline ..."}`),
			ResultText: "[clawvisor-notice] scope was expanded; do not re-emit",
		}
	}
	out, err := StripSyntheticApprovalHistory(SyntheticApprovalHistoryStripRequest{
		Provider:             conversation.ProviderAnthropic,
		Body:                 body,
		ReconstructionLookup: lookup,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Modified {
		t.Fatalf("strip should rewrite the body for reconstruction; got unchanged: %s", out.Body)
	}
	got := string(out.Body)
	// The substituted-prompt text and AskUserQuestion tool_use_id
	// must NOT survive — that's what we're replacing.
	if strings.Contains(got, "Clawvisor wants to expand") {
		t.Errorf("substituted-prompt text leaked: %s", got)
	}
	if strings.Contains(got, askToolUseID) {
		t.Errorf("AskUserQuestion tool_use_id leaked: %s", got)
	}
	// The reconstructed tool_use_id and notice MUST appear.
	if !strings.Contains(got, originalToolUseID) {
		t.Errorf("reconstructed tool_use_id missing: %s", got)
	}
	if !strings.Contains(got, "scope was expanded; do not re-emit") {
		t.Errorf("reconstructed ResultText missing: %s", got)
	}
	if !strings.Contains(got, "curl -X POST") {
		t.Errorf("reconstructed tool_input missing: %s", got)
	}
}

// TestStripSyntheticApprovalHistory_ReconstructionIdempotentAcrossTurns
// confirms a second strip pass on an already-reconstructed body is
// a no-op. Without idempotency a persistent-reconstruction loop
// could re-strip the synthetic pair (no Clawvisor marker on it, so
// the detector shouldn't fire) — but pin it explicitly.
func TestStripSyntheticApprovalHistory_ReconstructionIdempotentAcrossTurns(t *testing.T) {
	const approvalID = "cv-persistidempo1"
	const askToolUseID = "toolu_clawvisor_ask_" + approvalID
	const originalToolUseID = "toolu_01ReconstructIdempo"
	bodyV1, err := json.Marshal(map[string]any{
		"model": "claude-haiku-4-5",
		"messages": []map[string]any{
			{"role": "user", "content": "expand the task"},
			{"role": "assistant", "content": []map[string]any{
				{"type": "text", "text": "Clawvisor wants to expand the scope of an existing task:\n[clawvisor:approval=" + approvalID + "]"},
				{"type": "tool_use", "id": askToolUseID, "name": "AskUserQuestion", "input": map[string]any{
					"questions": []map[string]any{{"question": "approve?"}},
				}},
			}},
			{"role": "user", "content": []map[string]any{
				{"type": "tool_result", "tool_use_id": askToolUseID, "content": "yes"},
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	lookup := func(id string) *historystrip.ReconstructedPair {
		if id != approvalID {
			return nil
		}
		return &historystrip.ReconstructedPair{
			ToolUseID:  originalToolUseID,
			ToolName:   "Bash",
			Input:      json.RawMessage(`{"command":"curl ..."}`),
			ResultText: "scope expanded",
		}
	}
	// First pass: should reconstruct.
	pass1, err := StripSyntheticApprovalHistory(SyntheticApprovalHistoryStripRequest{
		Provider:             conversation.ProviderAnthropic,
		Body:                 bodyV1,
		ReconstructionLookup: lookup,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !pass1.Modified {
		t.Fatalf("first pass should reconstruct, got: %s", pass1.Body)
	}
	// Second pass on the already-reconstructed body should be a
	// no-op: no Clawvisor marker remains in the assistant text.
	pass2, err := StripSyntheticApprovalHistory(SyntheticApprovalHistoryStripRequest{
		Provider:             conversation.ProviderAnthropic,
		Body:                 pass1.Body,
		ReconstructionLookup: lookup,
	})
	if err != nil {
		t.Fatal(err)
	}
	if pass2.Modified {
		t.Errorf("second pass on reconstructed body should be a no-op: %s", pass2.Body)
	}
	if string(pass1.Body) != string(pass2.Body) {
		t.Errorf("idempotency broken: pass1 != pass2\npass1=%s\npass2=%s", pass1.Body, pass2.Body)
	}
}

// TestStripSyntheticApprovalHistory_WrapsTextOnlyUserTurnAsToolResult
// pins the fix for the text-shape inline-approval path: when the
// substituted-prompt assistant turn had no AskUserQuestion (Codex,
// Telegram-bot agents, any harness without an AskUserQuestion-style
// picker), the body editor lands the approval notice as a plain
// string user content. After the strip reconstructs the assistant
// turn into [tool_use(original)], that user turn would dangle as an
// unpaired text message and Anthropic would 400 with
// "messages.N: tool_use ids were found without tool_result blocks
// immediately after". The strip must wrap the notice into a
// tool_result paired to the reconstructed tool_use_id.
func TestStripSyntheticApprovalHistory_WrapsTextOnlyUserTurnAsToolResult(t *testing.T) {
	const approvalID = "cv-textwrapcurr01"
	const originalToolUseID = "toolu_01TextOnlyOriginal"
	notice := `<clawvisor-notice kind="task-approved">Task was created and approved by the user. Task ID: task-x.</clawvisor-notice>`
	body, err := json.Marshal(map[string]any{
		"model": "claude-haiku-4-5",
		"messages": []map[string]any{
			{"role": "user", "content": "create the task"},
			{"role": "assistant", "content": []map[string]any{
				{"type": "text", "text": "Clawvisor wants to create a task to cover this work:\n\nPurpose\n  Bootstrap\n\n[clawvisor:approval=" + approvalID + "]"},
			}},
			// Plain-string content: the body editor's text-shape
			// rewrite (replaceAnthropicApprovalReply) produces this
			// shape after a user "y" / "approve" reply.
			{"role": "user", "content": notice},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	lookup := func(id string) *historystrip.ReconstructedPair {
		if id != approvalID {
			return nil
		}
		return &historystrip.ReconstructedPair{
			ToolUseID:  originalToolUseID,
			ToolName:   "exec",
			Input:      json.RawMessage(`{"command":"curl -X POST .../control/tasks?surface=inline ..."}`),
			ResultText: notice,
		}
	}
	out, err := StripSyntheticApprovalHistory(SyntheticApprovalHistoryStripRequest{
		Provider:             conversation.ProviderAnthropic,
		Body:                 body,
		ReconstructionLookup: lookup,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Modified {
		t.Fatalf("strip should rewrite body for text-shape reconstruction; got unchanged: %s", out.Body)
	}
	got := string(out.Body)
	// Substituted-prompt text must not survive.
	if strings.Contains(got, "Clawvisor wants to create a task") {
		t.Errorf("substituted-prompt text leaked: %s", got)
	}
	// Assistant turn becomes [tool_use(original)].
	if !strings.Contains(got, `"id":"`+originalToolUseID+`"`) {
		t.Errorf("reconstructed tool_use_id missing: %s", got)
	}
	// User turn must be wrapped as a tool_result paired to the
	// reconstructed tool_use_id — this is the adjacency Anthropic
	// validates. Without the wrap, the user turn would be a plain
	// string and the next request would 400.
	if !strings.Contains(got, `"type":"tool_result"`) {
		t.Errorf("expected tool_result wrap on user turn; got: %s", got)
	}
	if !strings.Contains(got, `"tool_use_id":"`+originalToolUseID+`"`) {
		t.Errorf("tool_result must pair against reconstructed tool_use_id; got: %s", got)
	}
	// The notice text must round-trip into the tool_result's content.
	if !strings.Contains(got, "Task was created and approved") {
		t.Errorf("notice text missing from rewritten body: %s", got)
	}
	if !json.Valid(out.Body) {
		t.Errorf("rewritten body not valid JSON: %s", got)
	}
}

// TestStripSyntheticApprovalHistory_WrapsTextBlockUserTurnAsToolResult
// covers the persistent-augment path: on turn N+1 (and later) the
// client echoes back the original "approve" verb, the augmenter
// splices the notice into the user text block, and the strip then
// reconstructs the older assistant turn. The user content is an
// ARRAY of text blocks (notice + sibling system reminders), not a
// plain string — the wrap must move the notice into the tool_result
// while keeping unrelated text blocks alongside.
func TestStripSyntheticApprovalHistory_WrapsTextBlockUserTurnAsToolResult(t *testing.T) {
	const approvalID = "cv-textwrapblks02"
	const originalToolUseID = "toolu_01TextBlocksOrig"
	notice := `<clawvisor-notice kind="task-approved">Task was created. Task ID: task-y.</clawvisor-notice>`
	body, err := json.Marshal(map[string]any{
		"model": "claude-haiku-4-5",
		"messages": []map[string]any{
			{"role": "user", "content": "do the thing"},
			{"role": "assistant", "content": []map[string]any{
				{"type": "text", "text": "Clawvisor wants to create a task to cover this work:\n\n[clawvisor:approval=" + approvalID + "]"},
			}},
			{"role": "user", "content": []map[string]any{
				{"type": "text", "text": notice},
				{"type": "text", "text": "<system-reminder>preserve me</system-reminder>"},
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	lookup := func(id string) *historystrip.ReconstructedPair {
		if id != approvalID {
			return nil
		}
		return &historystrip.ReconstructedPair{
			ToolUseID:  originalToolUseID,
			ToolName:   "exec",
			Input:      json.RawMessage(`{"command":"curl ..."}`),
			ResultText: notice,
		}
	}
	out, err := StripSyntheticApprovalHistory(SyntheticApprovalHistoryStripRequest{
		Provider:             conversation.ProviderAnthropic,
		Body:                 body,
		ReconstructionLookup: lookup,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Modified {
		t.Fatalf("strip should rewrite body for text-blocks reconstruction; got unchanged: %s", out.Body)
	}
	got := string(out.Body)
	if !strings.Contains(got, `"type":"tool_result"`) {
		t.Errorf("expected tool_result wrap; got: %s", got)
	}
	if !strings.Contains(got, `"tool_use_id":"`+originalToolUseID+`"`) {
		t.Errorf("tool_result must pair against reconstructed tool_use_id; got: %s", got)
	}
	// The system-reminder text block must survive alongside the
	// tool_result — the harness relies on it for context.
	if !strings.Contains(got, "preserve me") {
		t.Errorf("sibling system-reminder text block must survive the wrap; got: %s", got)
	}
	if !json.Valid(out.Body) {
		t.Errorf("rewritten body not valid JSON: %s", got)
	}
}

// TestStripSyntheticApprovalHistory_NoOrphanWhenReconstructionUnavailable
// pins the safety property: when the reconstruction lookup returns
// nil (no lifecycle audit data, store outage, predates the audit),
// the strip falls back to drop-the-turn. A text-shape user turn
// must NOT get wrapped as a tool_result in that case — there's no
// reconstructed tool_use_id to pair against, so the wrap would
// introduce a fresh orphan and 400 the next request.
func TestStripSyntheticApprovalHistory_NoOrphanWhenReconstructionUnavailable(t *testing.T) {
	const approvalID = "cv-textnorec0003a"
	notice := `<clawvisor-notice kind="task-approved">Task was created.</clawvisor-notice>`
	body, err := json.Marshal(map[string]any{
		"model": "claude-haiku-4-5",
		"messages": []map[string]any{
			{"role": "user", "content": "create the task"},
			{"role": "assistant", "content": []map[string]any{
				{"type": "text", "text": "Clawvisor wants to create a task to cover this work:\n\n[clawvisor:approval=" + approvalID + "]"},
			}},
			{"role": "user", "content": notice},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	lookup := func(id string) *historystrip.ReconstructedPair { return nil }
	out, err := StripSyntheticApprovalHistory(SyntheticApprovalHistoryStripRequest{
		Provider:             conversation.ProviderAnthropic,
		Body:                 body,
		ReconstructionLookup: lookup,
	})
	if err != nil {
		t.Fatal(err)
	}
	got := string(out.Body)
	// No tool_result anywhere — wrapping without a reconstruction
	// would orphan immediately.
	if strings.Contains(got, `"type":"tool_result"`) {
		t.Errorf("no reconstruction available — must not wrap as tool_result; got: %s", got)
	}
	// User notice must still survive as text (not stripped) since
	// it's not a bare verb.
	if !strings.Contains(got, "Task was created") {
		t.Errorf("notice text must pass through unchanged: %s", got)
	}
}

func TestStripSyntheticApprovalHistory_KeepsSiblingTextBlocksAfterStrippingOrphanToolResult(t *testing.T) {
	// Real Claude Code shape: the harness packs the next-turn
	// system-reminders alongside the AskUserQuestion tool_result
	// in the same user message. When we strip the orphan
	// tool_result we must keep the sibling text blocks; dropping
	// the whole message would lose the system-reminders the model
	// needs.
	const approvalID = "cv-askuq-strip-2"
	body, err := json.Marshal(map[string]any{
		"model": "claude-haiku-4-5",
		"messages": []map[string]any{
			{"role": "user", "content": "Can you create a haiku file?"},
			{"role": "assistant", "content": []map[string]any{
				{"type": "text", "text": "Clawvisor wants to create a task to cover this work:\n\nPurpose\n  Create haiku\n\n[clawvisor:approval=" + approvalID + "]"},
				{"type": "tool_use", "id": "toolu_clawvisor_ask_" + approvalID, "name": "AskUserQuestion", "input": map[string]any{
					"questions": []map[string]any{{"question": "Approve this task?"}},
				}},
			}},
			{"role": "user", "content": []map[string]any{
				{"type": "text", "text": "<system-reminder>important context</system-reminder>"},
				{"type": "tool_result", "tool_use_id": "toolu_clawvisor_ask_" + approvalID, "content": "Your questions have been answered: \"Approve this task?\"=\"yes\". You can now continue with these answers in mind."},
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := StripSyntheticApprovalHistory(SyntheticApprovalHistoryStripRequest{
		Provider: conversation.ProviderAnthropic,
		Body:     body,
	})
	if err != nil {
		t.Fatal(err)
	}
	got := string(out.Body)
	if strings.Contains(got, "tool_result") {
		t.Fatalf("orphan tool_result should be stripped, got: %s", got)
	}
	if !strings.Contains(got, "important context") {
		t.Fatalf("sibling text block must survive the strip, got: %s", got)
	}
}

func TestStripSyntheticApprovalHistory_KeepsInlineOutcomeContext(t *testing.T) {
	note := inlineApprovedReplyAugmentation()
	body := anthropicTextBody(
		map[string]string{"role": "user", "content": "Create /tmp/hello.py"},
		map[string]string{"role": "assistant", "content": promptWithFooter("cv-approve-1", "Create /tmp/hello.py")},
		map[string]string{"role": "user", "content": note},
	)

	out, err := StripSyntheticApprovalHistory(SyntheticApprovalHistoryStripRequest{
		Provider: conversation.ProviderAnthropic,
		Body:     body,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Modified {
		t.Fatal("expected synthetic prompt to be stripped")
	}
	text := string(out.Body)
	if strings.Contains(text, InlineApprovalSubstitutedPromptMarker) {
		t.Fatalf("approval prompt leaked upstream: %s", text)
	}
	if !strings.Contains(text, inlineTaskNoticeOpenPrefixJSON) {
		t.Fatalf("inline outcome context should remain: %s", text)
	}
	var decoded struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out.Body, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Messages) != 1 {
		t.Fatalf("expected consecutive user messages to be merged; got %d messages: %+v", len(decoded.Messages), decoded.Messages)
	}
	got := flattenAnthropicTaskReplyText(decoded.Messages[0].Content)
	if !strings.Contains(got, "Create /tmp/hello.py") || !strings.Contains(got, note) {
		t.Fatalf("merged message missing original content or note: %q", got)
	}
}

func TestStripSyntheticApprovalHistory_DoesNotPatchAnthropicByModelNameByDefault(t *testing.T) {
	body := []byte(`{
		"model": "openai/gpt-oss-120b:free",
		"thinking": {"type": "disabled"},
		"messages": [{
			"role": "user",
			"content": [{"type": "text", "text": "hi", "cache_control": {"type": "ephemeral"}}]
		}]
	}`)

	out, err := StripSyntheticApprovalHistory(SyntheticApprovalHistoryStripRequest{
		Provider: conversation.ProviderAnthropic,
		Body:     body,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Modified {
		t.Fatal("expected default strip path not to mutate Anthropic-compatible body based on model name")
	}
	if got := string(out.Body); !strings.Contains(got, "thinking") || !strings.Contains(got, "cache_control") {
		t.Fatalf("expected thinking and cache_control to remain, got: %s", got)
	}
}

func TestStripSyntheticApprovalHistory_PreservesMixedAnthropicContentWithoutReshapingBlocks(t *testing.T) {
	body, err := json.Marshal(map[string]any{
		"model": "claude-test",
		"messages": []map[string]any{
			{
				"role": "user",
				"content": []map[string]any{
					{"type": "text", "text": "Create /tmp/hello.py"},
					{"type": "image", "source": map[string]string{"type": "base64", "media_type": "image/png", "data": "abc"}},
				},
			},
			{"role": "assistant", "content": InlineApprovalSubstitutedPromptMarker + "\n\nReply approve or deny."},
			{"role": "user", "content": inlineApprovedReplyAugmentation()},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	out, err := StripSyntheticApprovalHistory(SyntheticApprovalHistoryStripRequest{
		Provider: conversation.ProviderAnthropic,
		Body:     body,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Modified {
		t.Fatal("expected synthetic prompt to be stripped")
	}
	var decoded struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out.Body, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Messages) != 2 {
		t.Fatalf("expected mixed structured/string user content to remain as separate messages, got %d: %s", len(decoded.Messages), out.Body)
	}
	var blocks []map[string]interface{}
	if err := json.Unmarshal(decoded.Messages[0].Content, &blocks); err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 2 {
		t.Fatalf("expected original structured blocks to remain unchanged, got %+v", blocks)
	}
	if blocks[1]["type"] != "image" {
		t.Fatalf("non-text content block was corrupted: %+v", blocks)
	}
	var outcome string
	if err := json.Unmarshal(decoded.Messages[1].Content, &outcome); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(outcome, inlineTaskNoticeOpenPrefix) {
		t.Fatalf("inline outcome context missing from second message: %q", outcome)
	}
}

func TestStripSyntheticApprovalHistory_DropsToolPromptAndBareReply(t *testing.T) {
	body := anthropicTextBody(
		map[string]string{"role": "user", "content": "Run ls"},
		map[string]string{"role": "assistant", "content": ToolApprovalSubstitutedPromptMarker + "\n\nTool: `Bash`\nInput: ls\n\nReply `(y)es` to run this tool call."},
		map[string]string{"role": "user", "content": "no"},
	)

	out, err := StripSyntheticApprovalHistory(SyntheticApprovalHistoryStripRequest{
		Provider: conversation.ProviderAnthropic,
		Body:     body,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Modified {
		t.Fatal("expected synthetic tool prompt history to be stripped")
	}
	text := string(out.Body)
	if strings.Contains(text, ToolApprovalSubstitutedPromptMarker) || strings.Contains(text, `"no"`) {
		t.Fatalf("synthetic tool approval history leaked upstream: %s", text)
	}
}

func TestStripSyntheticApprovalHistory_DoesNotTouchUserMention(t *testing.T) {
	body := anthropicTextBody(
		map[string]string{"role": "user", "content": "Why did it say " + InlineApprovalSubstitutedPromptMarker + "?"},
	)

	out, err := StripSyntheticApprovalHistory(SyntheticApprovalHistoryStripRequest{
		Provider: conversation.ProviderAnthropic,
		Body:     body,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Modified {
		t.Fatalf("user-authored diagnostic text should be preserved: %s", out.Body)
	}
}

func TestStripSyntheticApprovalHistory_IntermediateUserTurnDoesNotCorruptReconstruction(t *testing.T) {
	const approvalID = "cv-persistreconst1"
	const askToolUseID = "toolu_clawvisor_ask_" + approvalID
	const originalToolUseID = "toolu_01OriginalReconstruct"
	body, err := json.Marshal(map[string]any{
		"model": "claude-haiku-4-5",
		"messages": []map[string]any{
			{"role": "user", "content": "expand the task"},
			{"role": "assistant", "content": []map[string]any{
				{"type": "text", "text": "Clawvisor wants to expand the scope of an existing task:\n[clawvisor:approval=" + approvalID + "]"},
				{"type": "tool_use", "id": askToolUseID, "name": "AskUserQuestion", "input": map[string]any{
					"questions": []map[string]any{{"question": "approve?"}},
				}},
			}},
			{"role": "user", "content": "System warning: rate limit reached soon"}, // intermediate turn
			{"role": "user", "content": []map[string]any{
				{"type": "tool_result", "tool_use_id": askToolUseID, "content": "yes"},
			}},
			{"role": "assistant", "content": "Got it, proceeding."},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	lookup := func(id string) *historystrip.ReconstructedPair {
		if id != approvalID {
			return nil
		}
		return &historystrip.ReconstructedPair{
			ToolUseID:  originalToolUseID,
			ToolName:   "Bash",
			Input:      json.RawMessage(`{"command":"curl -X POST .../expand?surface=inline ..."}`),
			ResultText: "[clawvisor-notice] scope was expanded; do not re-emit",
		}
	}
	out, err := StripSyntheticApprovalHistory(SyntheticApprovalHistoryStripRequest{
		Provider:             conversation.ProviderAnthropic,
		Body:                 body,
		ReconstructionLookup: lookup,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Modified {
		t.Fatal("expected history strip to modify body")
	}
	got := string(out.Body)
	if !strings.Contains(got, originalToolUseID) {
		t.Errorf("expected reconstructed tool_use_id %q to be present: %s", originalToolUseID, got)
	}
	if !strings.Contains(got, "[clawvisor-notice] scope was expanded") {
		t.Errorf("expected reconstructed tool_result content to be present: %s", got)
	}
}
