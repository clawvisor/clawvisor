package llmproxy

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
)

// Build an Anthropic-shape request body with the given user/assistant
// text-only messages. Useful for asserting against augmentation rewrites.
func anthropicTextBody(messages ...map[string]string) []byte {
	out := map[string]any{"model": "claude-haiku-4-5", "messages": []map[string]any{}}
	for _, m := range messages {
		out["messages"] = append(out["messages"].([]map[string]any), map[string]any{
			"role":    m["role"],
			"content": m["content"],
		})
	}
	b, _ := json.Marshal(out)
	return b
}

// promptWithFooter builds an inline-prompt assistant message with the
// approval-id footer the augmenter parses to look up the outcome.
func promptWithFooter(approvalID, purposeLine string) string {
	body := "Clawvisor wants to create a task to cover this work:\n\nPurpose\n  " + purposeLine + "\n\nReply approve to authorize, deny to cancel."
	return body + "\n\n" + InlineApprovalIDMarker + approvalID + "]"
}

func TestAugment_InjectsContextOnBareApproveAfterSubstitutedPrompt(t *testing.T) {
	outcomes := NewMemoryInlineApprovalOutcomeStore(time.Minute)
	outcomes.Record(InlineApprovalOutcomeKey{UserID: "user-1", AgentID: "agent-1", ApprovalID: "cv-approve-1"}, InlineApprovalOutcome{Succeeded: true, TaskID: "task-abc"})

	body := anthropicTextBody(
		map[string]string{"role": "user", "content": "Can you create a series of fake LLM conversations in /tmp/x?"},
		map[string]string{"role": "assistant", "content": promptWithFooter("cv-approve-1", "Create /tmp/x ...")},
		map[string]string{"role": "user", "content": "approve"},
		map[string]string{"role": "assistant", "content": "Running mkdir..."},
		map[string]string{"role": "user", "content": "mkdir output"},
	)
	out, rewritten, err := AugmentApprovedInlineTasksInHistory(body, conversation.ProviderAnthropic, outcomes, "user-1", "agent-1")
	if err != nil {
		t.Fatal(err)
	}
	if !rewritten {
		t.Fatal("expected augmentation to fire on bare approve after substituted prompt")
	}
	s := string(out)
	if !strings.Contains(s, InlineApprovalAugmentationMarker) {
		t.Fatalf("output missing augmentation marker: %s", s)
	}
	if !strings.Contains(s, "Do NOT POST /control/tasks again") {
		t.Fatalf("output missing do-not-repost guidance: %s", s)
	}
	// The augmented user content is just the bracketed note — no
	// leading "approve" verb. Earlier code prepended "approve\n\n",
	// which left a parseable bare-verb line in the augmented body.
	// The bracketed text already conveys what the user did.
	if !strings.Contains(s, `"[Clawvisor`) {
		t.Fatalf("augmentation context missing: %s", s)
	}
}

func TestAugment_IdempotentOnSecondPass(t *testing.T) {
	outcomes := NewMemoryInlineApprovalOutcomeStore(time.Minute)
	outcomes.Record(InlineApprovalOutcomeKey{UserID: "user-1", AgentID: "agent-1", ApprovalID: "cv-approve-1"}, InlineApprovalOutcome{Succeeded: true, TaskID: "task-abc"})

	body := anthropicTextBody(
		map[string]string{"role": "user", "content": "x"},
		map[string]string{"role": "assistant", "content": promptWithFooter("cv-approve-1", "...")},
		map[string]string{"role": "user", "content": "approve"},
	)
	first, ok1, _ := AugmentApprovedInlineTasksInHistory(body, conversation.ProviderAnthropic, outcomes, "user-1", "agent-1")
	if !ok1 {
		t.Fatal("first pass should augment")
	}
	second, ok2, _ := AugmentApprovedInlineTasksInHistory(first, conversation.ProviderAnthropic, outcomes, "user-1", "agent-1")
	if ok2 {
		t.Fatal("second pass on already-augmented body should be a no-op")
	}
	if string(second) != string(first) {
		t.Fatal("second pass should not modify body")
	}
}

func TestAugment_NoopOnRegularToolApprove(t *testing.T) {
	body := anthropicTextBody(
		map[string]string{"role": "user", "content": "Do something"},
		map[string]string{"role": "assistant", "content": "Clawvisor paused this tool call for approval.\n\nTool: Bash\nInput: ls\n\nReply approve to run, deny to block, or task to ..."},
		map[string]string{"role": "user", "content": "approve"},
	)
	_, ok, err := AugmentApprovedInlineTasksInHistory(body, conversation.ProviderAnthropic, NewMemoryInlineApprovalOutcomeStore(time.Minute), "user-1", "agent-1")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("regular tool-stage approve must NOT trigger inline-task augmentation")
	}
}

func TestAugment_HandlesMultipleApprovesInHistory(t *testing.T) {
	outcomes := NewMemoryInlineApprovalOutcomeStore(time.Minute)
	outcomes.Record(InlineApprovalOutcomeKey{UserID: "user-1", AgentID: "agent-1", ApprovalID: "cv-approve-A"}, InlineApprovalOutcome{Succeeded: true, TaskID: "task-a"})
	outcomes.Record(InlineApprovalOutcomeKey{UserID: "user-1", AgentID: "agent-1", ApprovalID: "cv-approve-B"}, InlineApprovalOutcome{Succeeded: true, TaskID: "task-b"})

	body := anthropicTextBody(
		map[string]string{"role": "user", "content": "Create files in /tmp/x"},
		map[string]string{"role": "assistant", "content": promptWithFooter("cv-approve-A", "first")},
		map[string]string{"role": "user", "content": "approve"},
		map[string]string{"role": "assistant", "content": "Running mkdir..."},
		map[string]string{"role": "user", "content": "mkdir output"},
		map[string]string{"role": "assistant", "content": promptWithFooter("cv-approve-B", "second")},
		map[string]string{"role": "user", "content": "approve"},
	)
	out, ok, _ := AugmentApprovedInlineTasksInHistory(body, conversation.ProviderAnthropic, outcomes, "user-1", "agent-1")
	if !ok {
		t.Fatal("expected augmentation")
	}
	count := strings.Count(string(out), InlineApprovalAugmentationMarker)
	if count != 2 {
		t.Errorf("expected 2 augmented approves; got %d markers in body=%s", count, out)
	}
}

func TestAugment_BlockContentDoesNotLeaveTrailingBareApprove(t *testing.T) {
	outcomes := NewMemoryInlineApprovalOutcomeStore(time.Minute)
	outcomes.Record(InlineApprovalOutcomeKey{UserID: "user-1", AgentID: "agent-1", ApprovalID: "cv-approve-1"}, InlineApprovalOutcome{Succeeded: true, TaskID: "task-abc"})

	bodyMap := map[string]any{
		"model": "claude-haiku-4-5",
		"messages": []map[string]any{
			{"role": "assistant", "content": promptWithFooter("cv-approve-1", "Create /tmp/x ...")},
			{
				"role": "user",
				"content": []map[string]any{
					{"type": "text", "text": "prefix that arrived in an earlier text block"},
					{"type": "text", "text": "approve"},
				},
			},
		},
	}
	body, _ := json.Marshal(bodyMap)

	out, ok, err := AugmentApprovedInlineTasksInHistory(body, conversation.ProviderAnthropic, outcomes, "user-1", "agent-1")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected block-shaped approve to be augmented")
	}

	var decoded struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("augmented body is invalid JSON: %v\n%s", err, out)
	}
	var userContent []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(decoded.Messages[1].Content, &userContent); err != nil {
		t.Fatalf("augmented user message did not preserve content-block shape: %v\n%s", err, out)
	}
	if len(userContent) == 0 {
		t.Fatalf("expected at least one preserved/rewritten content block; got %+v", userContent)
	}
	hasMarker := false
	for _, block := range userContent {
		if strings.Contains(block.Text, InlineApprovalAugmentationMarker) {
			hasMarker = true
			break
		}
	}
	if !hasMarker {
		t.Fatalf("augmentation marker missing from blocks: %+v", userContent)
	}
	for _, block := range userContent {
		if block.Type == "text" && strings.TrimSpace(block.Text) == "approve" {
			t.Fatalf("bare approve block survived after augmentation: %+v", userContent)
		}
	}
	userContentRaw, err := json.Marshal(userContent)
	if err != nil {
		t.Fatal(err)
	}
	combined := flattenAnthropicTaskReplyText(userContentRaw)
	if verb, _ := conversation.ParseApprovalReplyText(combined); verb == "approve" {
		t.Fatalf("combined augmented text still parses as a fresh bare approve: %q", combined)
	}
}

func TestAugment_BlockContentStripsLaterApprovalAfterEarlierApprovalLikeBlock(t *testing.T) {
	outcomes := NewMemoryInlineApprovalOutcomeStore(time.Minute)
	outcomes.Record(InlineApprovalOutcomeKey{UserID: "user-1", AgentID: "agent-1", ApprovalID: "cv-approve-1"}, InlineApprovalOutcome{Succeeded: true, TaskID: "task-abc"})

	bodyMap := map[string]any{
		"model": "claude-haiku-4-5",
		"messages": []map[string]any{
			{"role": "assistant", "content": promptWithFooter("cv-approve-1", "Create /tmp/x ...")},
			{
				"role": "user",
				"content": []map[string]any{
					{"type": "text", "text": "deny cv-oldoldoldold"},
					{"type": "text", "text": "approve"},
				},
			},
		},
	}
	body, _ := json.Marshal(bodyMap)

	out, ok, err := AugmentApprovedInlineTasksInHistory(body, conversation.ProviderAnthropic, outcomes, "user-1", "agent-1")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected block-shaped approve to be augmented")
	}

	var decoded struct {
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("augmented body is invalid JSON: %v\n%s", err, out)
	}
	var userContent []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(decoded.Messages[1].Content, &userContent); err != nil {
		t.Fatalf("augmented user message did not preserve content-block shape: %v\n%s", err, out)
	}
	raw, err := json.Marshal(userContent)
	if err != nil {
		t.Fatal(err)
	}
	combined := flattenAnthropicTaskReplyText(raw)
	if verb, id := conversation.ParseApprovalReplyText(combined); verb != "" || id != "" {
		t.Fatalf("augmented content still parses as an approval: verb=%q id=%q text=%q", verb, id, combined)
	}
	for _, block := range userContent {
		if strings.TrimSpace(block.Text) == "approve" {
			t.Fatalf("later bare approve block survived after earlier approval-like block was rewritten: %+v", userContent)
		}
	}
}

func TestAugment_DoesNotTouchOtherUserMessages(t *testing.T) {
	body := anthropicTextBody(
		map[string]string{"role": "user", "content": "first message"},
		map[string]string{"role": "assistant", "content": "ok"},
		map[string]string{"role": "user", "content": "approve"},
	)
	out, ok, _ := AugmentApprovedInlineTasksInHistory(body, conversation.ProviderAnthropic, NewMemoryInlineApprovalOutcomeStore(time.Minute), "user-1", "agent-1")
	if ok {
		t.Fatal("bare approve without substituted prompt should NOT augment")
	}
	if string(out) != string(body) {
		t.Fatal("body should be unchanged when no qualifying approve")
	}
}

// A failed inline approval must NOT be re-rendered as success on later
// turns. Before the per-approval outcome store, the augmenter
// unconditionally injected "task was created and approved" whenever it
// saw a bare "approve" after the inline prompt — even when creation
// had actually failed (validation error, missing creator, store error).
func TestAugment_FailedApprovalGetsFailureContext(t *testing.T) {
	outcomes := NewMemoryInlineApprovalOutcomeStore(time.Minute)
	outcomes.Record(InlineApprovalOutcomeKey{UserID: "user-1", AgentID: "agent-1", ApprovalID: "cv-approve-fail"}, InlineApprovalOutcome{
		Succeeded:     false,
		FailureReason: "create failed: validation error",
	})

	body := anthropicTextBody(
		map[string]string{"role": "user", "content": "Set up a dangerous task"},
		map[string]string{"role": "assistant", "content": promptWithFooter("cv-approve-fail", "...")},
		map[string]string{"role": "user", "content": "approve"},
	)
	out, ok, err := AugmentApprovedInlineTasksInHistory(body, conversation.ProviderAnthropic, outcomes, "user-1", "agent-1")
	if err != nil || !ok {
		t.Fatalf("expected augmentation; ok=%v err=%v", ok, err)
	}
	s := string(out)
	if strings.Contains(s, "was created and approved") {
		t.Fatalf("failed approval was rendered as success: %s", s)
	}
	if !strings.Contains(s, "NOT completed") {
		t.Fatalf("failed-context note missing: %s", s)
	}
	if !strings.Contains(s, "validation error") {
		t.Fatalf("failure reason should be surfaced to the model: %s", s)
	}
}

// When the outcome is unknown (cache evicted, or prompt lacks the
// footer because it was rendered by an older daemon), the augmenter
// must NOT guess success. It skips augmentation so the model sees the
// honest history.
func TestAugment_UnknownOutcomeSkipsAugmentation(t *testing.T) {
	outcomes := NewMemoryInlineApprovalOutcomeStore(time.Minute)
	// No Record() — outcome is unknown.
	body := anthropicTextBody(
		map[string]string{"role": "user", "content": "x"},
		map[string]string{"role": "assistant", "content": promptWithFooter("cv-approve-unknown", "...")},
		map[string]string{"role": "user", "content": "approve"},
	)
	_, ok, _ := AugmentApprovedInlineTasksInHistory(body, conversation.ProviderAnthropic, outcomes, "user-1", "agent-1")
	if ok {
		t.Fatal("unknown outcome must NOT augment — would risk a false success claim")
	}
}

// Anthropic rejects requests with empty text blocks. When a user
// message has TWO verb-bearing text blocks, the splice-at block gets
// the note but the other was previously left as `{"type":"text",
// "text":""}` — a 400 from Anthropic. The fix drops empty blocks
// after stripping.
func TestAugment_BlockContentDropsEmptyBlocksAfterStrip(t *testing.T) {
	outcomes := NewMemoryInlineApprovalOutcomeStore(time.Minute)
	outcomes.Record(
		InlineApprovalOutcomeKey{UserID: "user-1", AgentID: "agent-1", ApprovalID: "cv-approve-1"},
		InlineApprovalOutcome{Succeeded: true, TaskID: "task-abc"},
	)

	bodyMap := map[string]any{
		"model": "claude-haiku-4-5",
		"messages": []map[string]any{
			{"role": "assistant", "content": promptWithFooter("cv-approve-1", "...")},
			{
				"role": "user",
				"content": []map[string]any{
					{"type": "text", "text": "deny cv-oldoldoldold"},
					{"type": "text", "text": "approve"},
				},
			},
		},
	}
	body, _ := json.Marshal(bodyMap)

	out, ok, err := AugmentApprovedInlineTasksInHistory(body, conversation.ProviderAnthropic, outcomes, "user-1", "agent-1")
	if err != nil || !ok {
		t.Fatalf("expected augmentation; err=%v ok=%v", err, ok)
	}

	var decoded struct {
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatal(err)
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(decoded.Messages[1].Content, &blocks); err != nil {
		t.Fatal(err)
	}
	for _, b := range blocks {
		if b.Type == "text" && b.Text == "" {
			t.Fatalf("empty text block survived; Anthropic would reject the request: %+v", blocks)
		}
	}
}

// Outcomes are scoped per (userID, agentID, approvalID). A model in
// one agent that learned an approval ID from another agent's session
// must NOT trigger augmentation in the wrong scope. Real authorization
// runs against the task store regardless; this is the defense-in-depth
// scoping the rest of the approval system uses.
func TestAugment_OutcomeLookupIsScopedPerAgent(t *testing.T) {
	outcomes := NewMemoryInlineApprovalOutcomeStore(time.Minute)
	// Outcome recorded under agent A.
	outcomes.Record(
		InlineApprovalOutcomeKey{UserID: "user-1", AgentID: "agent-A", ApprovalID: "cv-shared-id"},
		InlineApprovalOutcome{Succeeded: true, TaskID: "task-a"},
	)
	body := anthropicTextBody(
		map[string]string{"role": "user", "content": "x"},
		map[string]string{"role": "assistant", "content": promptWithFooter("cv-shared-id", "...")},
		map[string]string{"role": "user", "content": "approve"},
	)
	// Augmenter runs for agent B with the SAME approval ID — must miss.
	_, ok, _ := AugmentApprovedInlineTasksInHistory(body, conversation.ProviderAnthropic, outcomes, "user-1", "agent-B")
	if ok {
		t.Fatal("augmentation fired across agent boundary; scoping is broken")
	}
}

// FailureReason can contain model-controlled strings (task purpose,
// command echoes via createErr.Error()). A reason with `]` would
// prematurely close the [Clawvisor: …] bracket envelope; a reason
// with a newline + bare verb could resurrect a bare-approve line for
// a downstream re-parse. Strip both.
func TestInlineFailedReplyAugmentationContextSanitizesReason(t *testing.T) {
	cases := []struct {
		in      string
		mustNot []string
	}{
		{
			in:      "boom] (extra) ] more",
			mustNot: []string{"]"},
		},
		{
			in:      "line1\napprove\nline3",
			mustNot: []string{"\n"},
		},
	}
	for _, tc := range cases {
		got := inlineFailedReplyAugmentationContext(tc.in)
		// Trim the closing bracket the template adds at the end.
		body := strings.TrimSuffix(got, "]")
		for _, banned := range tc.mustNot {
			if strings.Contains(body, banned) {
				t.Errorf("reason %q produced augmentation containing %q: %s", tc.in, banned, got)
			}
		}
	}
}

// Drift guarantee: the one-shot rewrite and the persistent augmenter
// must produce byte-identical "approve" turns on the success path so
// the model never sees the same past message render two different ways.
// Both now reduce to the bracketed context with no verb prefix.
func TestAugment_OneShotAndPersistentProduceIdenticalText(t *testing.T) {
	oneShot := inlineApprovedReplyAugmentation()
	persistent := inlineApprovedReplyAugmentationContext(nil)
	if oneShot != persistent {
		t.Fatalf("one-shot and persistent renderings differ — drift bug.\n  one-shot:\n%s\n  persistent:\n%s", oneShot, persistent)
	}
}

func TestAugment_OpenAIProviderIsNoop(t *testing.T) {
	body := []byte(`{"input":"approve"}`)
	_, ok, err := AugmentApprovedInlineTasksInHistory(body, conversation.ProviderOpenAI, nil, "user-1", "agent-1")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("OpenAI provider is currently scoped out; should no-op")
	}
}
