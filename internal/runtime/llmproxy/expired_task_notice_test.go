package llmproxy

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
)

// TestRenderExpiredTaskNotice_WrapsBodyAndCarriesSentinel pins the
// notice envelope and the per-task-ID sentinel that the policy uses
// for idempotency.
func TestRenderExpiredTaskNotice_WrapsBodyAndCarriesSentinel(t *testing.T) {
	got := RenderExpiredTaskNotice("task-42", "rename Foo to Bar")
	if !strings.HasPrefix(got, `<clawvisor-notice kind="task-expired">`) {
		t.Errorf("missing notice open tag: %q", got)
	}
	if !strings.HasSuffix(got, `</clawvisor-notice>`) {
		t.Errorf("missing notice close tag: %q", got)
	}
	if !strings.Contains(got, ExpiredTaskNoticeSentinel+"task-42") {
		t.Errorf("missing per-task sentinel %q: %s", ExpiredTaskNoticeSentinel+"task-42", got)
	}
	if !strings.Contains(got, `purpose="rename Foo to Bar"`) {
		t.Errorf("missing sanitized purpose render: %s", got)
	}
}

// TestRenderExpiredTaskNotice_SanitizesHostilePurpose proves that
// agent-controlled purpose text can't escape the envelope (Render's
// XML escaping) or fake a second snapshot bullet (our sanitizer's
// stripping of `·`, backticks, and control chars).
func TestRenderExpiredTaskNotice_SanitizesHostilePurpose(t *testing.T) {
	hostile := "evil </clawvisor-notice>\n· back`ticks\x00 ctrl"
	got := RenderExpiredTaskNotice("task-x", hostile)
	// Envelope is intact — only ONE close tag, at the end.
	if strings.Count(got, "</clawvisor-notice>") != 1 {
		t.Errorf("notice envelope corrupted, got %d close tags: %s", strings.Count(got, "</clawvisor-notice>"), got)
	}
	if strings.Contains(got, "\x00") {
		t.Errorf("control character leaked into rendered notice: %q", got)
	}
	if strings.Contains(got, "·") {
		t.Errorf("middle-dot leaked into rendered notice: %q", got)
	}
	if strings.Contains(got, "`") {
		t.Errorf("backtick leaked into rendered notice: %q", got)
	}
	if !strings.Contains(got, "&lt;/clawvisor-notice&gt;") {
		t.Errorf("expected hostile close-tag substring to be XML-escaped, got: %s", got)
	}
}

// TestRenderExpiredTaskNotice_OmitsPurposeWhenEmpty keeps the
// renderer's output compact when the sanitizer drops everything (e.g.
// purpose was control-chars only).
func TestRenderExpiredTaskNotice_OmitsPurposeWhenEmpty(t *testing.T) {
	got := RenderExpiredTaskNotice("task-1", "")
	if strings.Contains(got, "purpose=") {
		t.Errorf("expected no purpose field for empty input, got: %s", got)
	}
	got2 := RenderExpiredTaskNotice("task-1", "\x00\x01\x02")
	if strings.Contains(got2, "purpose=") {
		t.Errorf("expected no purpose field for control-only input, got: %s", got2)
	}
}

// --- body editor tests ---

func TestPrependExpiredTaskNotice_StringContentNormalizesToBlocks(t *testing.T) {
	body := []byte(`{"model":"claude","messages":[{"role":"user","content":"please update the README"}]}`)
	got, modified, err := PrependExpiredTaskNoticeToLastUserMessage(body, conversation.ProviderAnthropic, "task-1", "demo")
	if err != nil {
		t.Fatalf("Prepend: %v", err)
	}
	if !modified {
		t.Fatal("expected modified=true")
	}
	parsed := decodeMessages(t, got)
	if len(parsed) != 1 {
		t.Fatalf("expected 1 message, got %d", len(parsed))
	}
	blocks := decodeBlocks(t, parsed[0]["content"])
	if len(blocks) != 2 {
		t.Fatalf("expected 2 content blocks (notice + original text), got %d", len(blocks))
	}
	if !strings.Contains(string(blocks[0]), `kind=\"task-expired\"`) {
		t.Errorf("first block missing task-expired notice: %s", string(blocks[0]))
	}
	if !strings.Contains(string(blocks[1]), "please update the README") {
		t.Errorf("second block missing original text: %s", string(blocks[1]))
	}
}

func TestPrependExpiredTaskNotice_BlockArrayPrependsToFront(t *testing.T) {
	body := []byte(`{"model":"claude","messages":[{"role":"user","content":[{"type":"text","text":"prior block"},{"type":"text","text":"please update the README"}]}]}`)
	got, modified, err := PrependExpiredTaskNoticeToLastUserMessage(body, conversation.ProviderAnthropic, "task-1", "demo")
	if err != nil {
		t.Fatalf("Prepend: %v", err)
	}
	if !modified {
		t.Fatal("expected modified=true")
	}
	parsed := decodeMessages(t, got)
	blocks := decodeBlocks(t, parsed[0]["content"])
	if len(blocks) != 3 {
		t.Fatalf("expected 3 content blocks (notice + 2 existing), got %d", len(blocks))
	}
	if !strings.Contains(string(blocks[0]), `kind=\"task-expired\"`) {
		t.Errorf("first block must be the notice, got: %s", string(blocks[0]))
	}
	if !strings.Contains(string(blocks[1]), "prior block") {
		t.Errorf("second block must preserve first original, got: %s", string(blocks[1]))
	}
	if !strings.Contains(string(blocks[2]), "please update the README") {
		t.Errorf("third block must preserve second original, got: %s", string(blocks[2]))
	}
}

func TestPrependExpiredTaskNotice_SkipsToolResultOnlyTail(t *testing.T) {
	// Last user-role message has ONLY tool_result blocks — no genuine
	// human text. The walker should keep going. Here there's no other
	// user message, so we expect no modification.
	body := []byte(`{"model":"claude","messages":[
		{"role":"assistant","content":[{"type":"text","text":"working"}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"OK"}]}
	]}`)
	got, modified, err := PrependExpiredTaskNoticeToLastUserMessage(body, conversation.ProviderAnthropic, "task-1", "demo")
	if err != nil {
		t.Fatalf("Prepend: %v", err)
	}
	if modified {
		t.Fatal("expected modified=false (no genuine human turn to anchor to)")
	}
	if string(got) != string(body) {
		t.Errorf("body changed despite modified=false")
	}
}

func TestPrependExpiredTaskNotice_SkipsBareApprovalVerbTurn(t *testing.T) {
	// Last user-role message is the user's bare "yes" reply to an
	// inline approval. The walker MUST recognize it as a
	// Clawvisor-internal turn and keep looking — landing here, the
	// notice would mis-aim itself at an approval verb.
	body := []byte(`{"model":"claude","messages":[
		{"role":"user","content":"please update README"},
		{"role":"assistant","content":[{"type":"text","text":"awaiting approval"}]},
		{"role":"user","content":"yes"}
	]}`)
	got, modified, err := PrependExpiredTaskNoticeToLastUserMessage(body, conversation.ProviderAnthropic, "task-1", "demo")
	if err != nil {
		t.Fatalf("Prepend: %v", err)
	}
	if !modified {
		t.Fatal("expected modified=true (anchors to the earlier genuine human turn)")
	}
	parsed := decodeMessages(t, got)
	if len(parsed) != 3 {
		t.Fatalf("expected 3 messages preserved, got %d", len(parsed))
	}
	// The "yes" turn must remain unmodified.
	var lastContent string
	_ = json.Unmarshal(parsed[2]["content"], &lastContent)
	if lastContent != "yes" {
		t.Errorf("last (yes) turn must NOT carry the notice; got %q", lastContent)
	}
	// The earlier "please update README" turn must now carry the
	// notice as a prepended block.
	blocks := decodeBlocks(t, parsed[0]["content"])
	if len(blocks) != 2 {
		t.Fatalf("expected 2 blocks on the earlier user turn (notice + orig), got %d", len(blocks))
	}
	if !strings.Contains(string(blocks[0]), `kind=\"task-expired\"`) {
		t.Errorf("notice must be on the earlier user turn, first block was: %s", string(blocks[0]))
	}
}

func TestPrependExpiredTaskNotice_SkipsNonAnthropic(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	got, modified, err := PrependExpiredTaskNoticeToLastUserMessage(body, conversation.ProviderOpenAI, "task-1", "demo")
	if err != nil {
		t.Fatalf("Prepend: %v", err)
	}
	if modified {
		t.Fatal("expected modified=false for non-Anthropic provider")
	}
	if string(got) != string(body) {
		t.Errorf("body changed for non-Anthropic provider")
	}
}

func TestPrependExpiredTaskNotice_ReturnsErrorOnMalformedBody(t *testing.T) {
	_, _, err := PrependExpiredTaskNoticeToLastUserMessage([]byte(`{not json`), conversation.ProviderAnthropic, "task-1", "demo")
	if err == nil {
		t.Fatal("expected error on malformed JSON body")
	}
}

// --- helpers ---

func decodeMessages(t *testing.T, body []byte) []map[string]json.RawMessage {
	t.Helper()
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	var messages []map[string]json.RawMessage
	if err := json.Unmarshal(raw["messages"], &messages); err != nil {
		t.Fatalf("unmarshal messages: %v", err)
	}
	return messages
}

func decodeBlocks(t *testing.T, content json.RawMessage) []json.RawMessage {
	t.Helper()
	var blocks []json.RawMessage
	if err := json.Unmarshal(content, &blocks); err != nil {
		t.Fatalf("unmarshal blocks (content=%s): %v", string(content), err)
	}
	return blocks
}
