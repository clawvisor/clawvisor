package llmproxy

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
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
	if !strings.Contains(text, InlineApprovalAugmentationMarker) {
		t.Fatalf("inline outcome context should remain: %s", text)
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
