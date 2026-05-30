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
	if !strings.Contains(outcome, InlineApprovalAugmentationMarker) {
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
