package llmproxy

import (
	"encoding/json"
	"strings"
	"testing"

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

func TestAugment_InjectsContextOnBareApproveAfterSubstitutedPrompt(t *testing.T) {
	body := anthropicTextBody(
		map[string]string{"role": "user", "content": "Can you create a series of fake LLM conversations in /tmp/x?"},
		map[string]string{"role": "assistant", "content": "Clawvisor wants to create a task to cover this work:\n\nPurpose\n  Create /tmp/x ...\n\nReply approve to authorize, deny to cancel."},
		map[string]string{"role": "user", "content": "approve"},
		map[string]string{"role": "assistant", "content": "Running mkdir..."},
		map[string]string{"role": "user", "content": "mkdir output"},
	)
	out, rewritten, err := AugmentApprovedInlineTasksInHistory(body, conversation.ProviderAnthropic)
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
	// The bare "approve" should still be present as the verb.
	if !strings.Contains(s, `"approve\n\n[Clawvisor`) {
		t.Fatalf("verb prefix lost or context not joined: %s", s)
	}
}

func TestAugment_IdempotentOnSecondPass(t *testing.T) {
	body := anthropicTextBody(
		map[string]string{"role": "user", "content": "x"},
		map[string]string{"role": "assistant", "content": "Clawvisor wants to create a task to cover this work:\n\n..."},
		map[string]string{"role": "user", "content": "approve"},
	)
	first, ok1, _ := AugmentApprovedInlineTasksInHistory(body, conversation.ProviderAnthropic)
	if !ok1 {
		t.Fatal("first pass should augment")
	}
	second, ok2, _ := AugmentApprovedInlineTasksInHistory(first, conversation.ProviderAnthropic)
	if ok2 {
		t.Fatal("second pass on already-augmented body should be a no-op")
	}
	if string(second) != string(first) {
		t.Fatal("second pass should not modify body")
	}
}

func TestAugment_NoopOnRegularToolApprove(t *testing.T) {
	// A bare "approve" that follows a tool-call approval prompt (not
	// the inline-task substituted prompt) should NOT trigger
	// augmentation — that path is a regular tool-stage approval
	// handled by TryReleasePendingApproval.
	body := anthropicTextBody(
		map[string]string{"role": "user", "content": "Do something"},
		map[string]string{"role": "assistant", "content": "Clawvisor paused this tool call for approval.\n\nTool: Bash\nInput: ls\n\nReply approve to run, deny to block, or task to ..."},
		map[string]string{"role": "user", "content": "approve"},
	)
	_, ok, err := AugmentApprovedInlineTasksInHistory(body, conversation.ProviderAnthropic)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("regular tool-stage approve must NOT trigger inline-task augmentation")
	}
}

func TestAugment_HandlesMultipleApprovesInHistory(t *testing.T) {
	// Production trace showed the model emitting a duplicate POST
	// /control/tasks after the first approve, getting intercepted
	// again, and the user approving twice. Both approves should be
	// augmented.
	body := anthropicTextBody(
		map[string]string{"role": "user", "content": "Create files in /tmp/x"},
		map[string]string{"role": "assistant", "content": "Clawvisor wants to create a task to cover this work:\n\nPurpose..."},
		map[string]string{"role": "user", "content": "approve"},
		map[string]string{"role": "assistant", "content": "Running mkdir..."},
		map[string]string{"role": "user", "content": "mkdir output"},
		map[string]string{"role": "assistant", "content": "Clawvisor wants to create a task to cover this work:\n\nPurpose..."},
		map[string]string{"role": "user", "content": "approve"},
	)
	out, ok, _ := AugmentApprovedInlineTasksInHistory(body, conversation.ProviderAnthropic)
	if !ok {
		t.Fatal("expected augmentation")
	}
	// Both approves should be augmented — count the marker.
	count := strings.Count(string(out), InlineApprovalAugmentationMarker)
	if count != 2 {
		t.Errorf("expected 2 augmented approves; got %d markers in body=%s", count, out)
	}
}

func TestAugment_DoesNotTouchOtherUserMessages(t *testing.T) {
	body := anthropicTextBody(
		map[string]string{"role": "user", "content": "first message"},
		map[string]string{"role": "assistant", "content": "ok"},
		map[string]string{"role": "user", "content": "approve"},
	)
	out, ok, _ := AugmentApprovedInlineTasksInHistory(body, conversation.ProviderAnthropic)
	if ok {
		t.Fatal("bare approve without substituted prompt should NOT augment")
	}
	if string(out) != string(body) {
		t.Fatal("body should be unchanged when no qualifying approve")
	}
}

func TestAugment_OpenAIProviderIsNoop(t *testing.T) {
	body := []byte(`{"input":"approve"}`)
	_, ok, err := AugmentApprovedInlineTasksInHistory(body, conversation.ProviderOpenAI)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("OpenAI provider is currently scoped out; should no-op")
	}
}
