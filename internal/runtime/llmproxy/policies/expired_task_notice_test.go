package policies_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/pipeline"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/policies"
)

// genuineUserBody is a minimal Anthropic request body whose last
// user-role message is a real human instruction. The notice's
// expected target.
const genuineUserBody = `{"model":"claude-sonnet-4","messages":[{"role":"user","content":"please update the README"}]}`

// notInjectingInjector is the test double for the body-mutation hook.
// It returns the body unchanged + modified=false, matching the
// behavior the real injector exhibits when there's no qualifying
// user-role message to anchor to.
func notInjectingInjector(body []byte, _ conversation.Provider, _, _ string) ([]byte, bool, error) {
	return body, false, nil
}

// constInjector returns a fixed replacement body and reports the
// mutation as performed. Lets the policy test exercise the
// Allow + audit path without parsing JSON itself.
func constInjector(replacement string) policies.ExpiredTaskNoticeInjector {
	return func(_ []byte, _ conversation.Provider, _, _ string) ([]byte, bool, error) {
		return []byte(replacement), true, nil
	}
}

func loaderExpired(taskID, purpose string) policies.ExpiredCheckedOutTaskLoader {
	return func(_ context.Context, _, _, _ string) (string, string, bool) {
		return taskID, purpose, true
	}
}

func loaderNotExpired() policies.ExpiredCheckedOutTaskLoader {
	return func(_ context.Context, _, _, _ string) (string, string, bool) {
		return "", "", false
	}
}

func newExpiredNoticeRequest(body string) *stubReadOnlyRequest {
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	return &stubReadOnlyRequest{
		provider:        conversation.ProviderAnthropic,
		rawBody:         []byte(body),
		userID:          "user-1",
		agentID:         "agent-1",
		httpReqOverride: httpReq,
	}
}

func TestExpiredTaskNotice_SkipsWhenLoaderNil(t *testing.T) {
	p := policies.NewExpiredTaskNotice(nil, constInjector(`{}`))
	mut := &recordingRequestMutator{}
	verdict, err := p.Preprocess(context.Background(), newExpiredNoticeRequest(genuineUserBody), mut)
	if err != nil {
		t.Fatalf("Preprocess: %v", err)
	}
	if verdict.Outcome != pipeline.OutcomeSkip {
		t.Fatalf("Outcome = %q, want Skip (nil loader)", verdict.Outcome)
	}
}

func TestExpiredTaskNotice_SkipsWhenInjectorNil(t *testing.T) {
	p := policies.NewExpiredTaskNotice(loaderExpired("task-1", "purpose"), nil)
	mut := &recordingRequestMutator{}
	verdict, _ := p.Preprocess(context.Background(), newExpiredNoticeRequest(genuineUserBody), mut)
	if verdict.Outcome != pipeline.OutcomeSkip {
		t.Errorf("Outcome = %q, want Skip (nil injector)", verdict.Outcome)
	}
}

func TestExpiredTaskNotice_SkipsWhenUserIDEmpty(t *testing.T) {
	p := policies.NewExpiredTaskNotice(loaderExpired("task-1", "purpose"), constInjector(`{}`))
	req := newExpiredNoticeRequest(genuineUserBody)
	req.userID = ""
	mut := &recordingRequestMutator{}
	verdict, _ := p.Preprocess(context.Background(), req, mut)
	if verdict.Outcome != pipeline.OutcomeSkip {
		t.Errorf("Outcome = %q, want Skip (empty userID)", verdict.Outcome)
	}
}

func TestExpiredTaskNotice_SkipsCountTokensPath(t *testing.T) {
	p := policies.NewExpiredTaskNotice(loaderExpired("task-1", "purpose"), constInjector(`{}`))
	req := newExpiredNoticeRequest(genuineUserBody)
	req.httpReqOverride.URL.Path = "/v1/messages/count_tokens"
	mut := &recordingRequestMutator{}
	verdict, _ := p.Preprocess(context.Background(), req, mut)
	if verdict.Outcome != pipeline.OutcomeSkip {
		t.Errorf("Outcome = %q, want Skip (count_tokens path)", verdict.Outcome)
	}
	if len(mut.ReplaceBodyCalls) != 0 {
		t.Errorf("ReplaceBody called %d times on count_tokens; want 0", len(mut.ReplaceBodyCalls))
	}
}

func TestExpiredTaskNotice_SkipsWhenNotExpired(t *testing.T) {
	p := policies.NewExpiredTaskNotice(loaderNotExpired(), constInjector(`{}`))
	mut := &recordingRequestMutator{}
	verdict, _ := p.Preprocess(context.Background(), newExpiredNoticeRequest(genuineUserBody), mut)
	if verdict.Outcome != pipeline.OutcomeSkip {
		t.Errorf("Outcome = %q, want Skip (loader reports not expired)", verdict.Outcome)
	}
}

func TestExpiredTaskNotice_InjectsWhenExpired(t *testing.T) {
	p := policies.NewExpiredTaskNotice(loaderExpired("task-1", "purpose"), constInjector(`{"replaced":true}`))
	mut := &recordingRequestMutator{}
	verdict, err := p.Preprocess(context.Background(), newExpiredNoticeRequest(genuineUserBody), mut)
	if err != nil {
		t.Fatalf("Preprocess: %v", err)
	}
	if verdict.Outcome != pipeline.OutcomeAllow {
		t.Fatalf("Outcome = %q, want Allow", verdict.Outcome)
	}
	if len(mut.ReplaceBodyCalls) != 1 {
		t.Fatalf("expected 1 ReplaceBody call, got %d", len(mut.ReplaceBodyCalls))
	}
	if string(mut.ReplaceBodyCalls[0]) != `{"replaced":true}` {
		t.Errorf("ReplaceBody payload = %q, want injector output", string(mut.ReplaceBodyCalls[0]))
	}
	if v, _ := verdict.AuditParams["expired_task_notice_injected"].(bool); !v {
		t.Errorf("audit flag expired_task_notice_injected = %v, want true", verdict.AuditParams["expired_task_notice_injected"])
	}
	if v, _ := verdict.AuditParams["expired_task_id"].(string); v != "task-1" {
		t.Errorf("audit field expired_task_id = %q, want task-1", v)
	}
}

func TestExpiredTaskNotice_SkipsWhenSentinelAlreadyPresent(t *testing.T) {
	// Body already carries the sentinel for the same task ID — the
	// previous turn's injection. Policy must NOT re-inject.
	body := `{"model":"claude-sonnet-4","messages":[{"role":"user","content":[{"type":"text","text":"<clawvisor-notice kind=\"task-expired\">prior (expired_task_id=task-1). recover...</clawvisor-notice>"},{"type":"text","text":"continue"}]}]}`
	p := policies.NewExpiredTaskNotice(loaderExpired("task-1", "purpose"), constInjector(`{"replaced":true}`))
	mut := &recordingRequestMutator{}
	verdict, _ := p.Preprocess(context.Background(), newExpiredNoticeRequest(body), mut)
	if verdict.Outcome != pipeline.OutcomeSkip {
		t.Fatalf("Outcome = %q, want Skip (sentinel present)", verdict.Outcome)
	}
	if len(mut.ReplaceBodyCalls) != 0 {
		t.Errorf("ReplaceBody called despite sentinel hit: %d calls", len(mut.ReplaceBodyCalls))
	}
}

func TestExpiredTaskNotice_InjectsForDifferentTaskIDDespiteExistingNotice(t *testing.T) {
	// A prior turn announced task-old. The agent has since checked out
	// task-new which has now also expired. Policy fires because the
	// per-task-ID sentinel doesn't match.
	body := `{"model":"claude-sonnet-4","messages":[{"role":"user","content":[{"type":"text","text":"<clawvisor-notice kind=\"task-expired\">prior (expired_task_id=task-old). recover...</clawvisor-notice>"},{"type":"text","text":"keep going"}]}]}`
	p := policies.NewExpiredTaskNotice(loaderExpired("task-new", "purpose"), constInjector(`{"replaced":true}`))
	mut := &recordingRequestMutator{}
	verdict, _ := p.Preprocess(context.Background(), newExpiredNoticeRequest(body), mut)
	if verdict.Outcome != pipeline.OutcomeAllow {
		t.Fatalf("Outcome = %q, want Allow for fresh task expiry", verdict.Outcome)
	}
	if len(mut.ReplaceBodyCalls) != 1 {
		t.Errorf("expected 1 ReplaceBody for fresh task, got %d", len(mut.ReplaceBodyCalls))
	}
}

func TestExpiredTaskNotice_SkipsWhenInjectorReportsNoModification(t *testing.T) {
	// Loader says expired, but the injector finds no anchor — e.g. the
	// last user-role messages are all tool_results. Skip cleanly.
	p := policies.NewExpiredTaskNotice(loaderExpired("task-1", "purpose"), notInjectingInjector)
	mut := &recordingRequestMutator{}
	verdict, _ := p.Preprocess(context.Background(), newExpiredNoticeRequest(genuineUserBody), mut)
	if verdict.Outcome != pipeline.OutcomeSkip {
		t.Errorf("Outcome = %q, want Skip (injector no-op)", verdict.Outcome)
	}
	if len(mut.ReplaceBodyCalls) != 0 {
		t.Errorf("ReplaceBody called %d times on injector no-op; want 0", len(mut.ReplaceBodyCalls))
	}
}

func TestExpiredTaskNotice_SkipsOnInjectorError(t *testing.T) {
	// Malformed body or other transient injector failure: record the
	// error in audit but Skip rather than Deny, so we don't turn an
	// observability glitch into a model-facing refusal.
	failing := func(_ []byte, _ conversation.Provider, _, _ string) ([]byte, bool, error) {
		return nil, false, errors.New("boom")
	}
	p := policies.NewExpiredTaskNotice(loaderExpired("task-1", "purpose"), failing)
	mut := &recordingRequestMutator{}
	verdict, err := p.Preprocess(context.Background(), newExpiredNoticeRequest(genuineUserBody), mut)
	if err != nil {
		t.Fatalf("Preprocess returned err: %v", err)
	}
	if verdict.Outcome != pipeline.OutcomeSkip {
		t.Errorf("Outcome = %q, want Skip on injector error", verdict.Outcome)
	}
	if got, _ := verdict.AuditParams["expired_task_notice_error"].(string); got != "boom" {
		t.Errorf("audit error field = %q, want boom", got)
	}
}
