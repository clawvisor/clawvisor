package llmproxy

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	runtimetasks "github.com/clawvisor/clawvisor/internal/runtime/tasks"
	"github.com/clawvisor/clawvisor/pkg/store"
)

func TestApprovalRoutingCharacterization_ExplicitInlineIDBeatsNewerToolHold(t *testing.T) {
	ctx := context.Background()
	cache := NewMemoryPendingApprovalCache(time.Minute)

	inlineHeld, err := cache.Hold(ctx, PendingLiteApproval{
		ID:       "cv-aaaaaaaaaaaaaaaaaaaaaaaaaa",
		UserID:   "user-1",
		AgentID:  "agent-1",
		Provider: conversation.ProviderAnthropic,
		Stage:    StageAwaitingTaskApproval,
		ToolUse:  conversation.ToolUse{ID: "toolu_post", Name: "Bash"},
		TaskDefinition: &runtimetasks.TaskCreateRequest{
			Purpose: "Create a landing page",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	toolHeld, err := cache.Hold(ctx, PendingLiteApproval{
		ID:       "cv-bbbbbbbbbbbbbbbbbbbbbbbbbb",
		UserID:   "user-1",
		AgentID:  "agent-1",
		Provider: conversation.ProviderAnthropic,
		Stage:    StageTool,
		ToolUse:  conversation.ToolUse{ID: "toolu_newer", Name: "Bash"},
	})
	if err != nil {
		t.Fatal(err)
	}

	creator := &fakeInlineTaskCreator{
		resp: &InlineApprovedTask{ID: "task-explicit-inline", ApprovalSource: "inline_chat"},
	}
	body := []byte(`{"messages":[{"role":"user","content":"approve ` + inlineHeld.Pending.ID + `"}]}`)
	out, err := RewriteInlineTaskApprovalReply(ctx, InlineApprovalRewriteRequest{
		HTTPRequest:     httptest.NewRequest("POST", "/v1/messages", nil),
		Provider:        conversation.ProviderAnthropic,
		Body:            body,
		Agent:           &store.Agent{ID: "agent-1", UserID: "user-1"},
		PendingApproval: cache,
		Creator:         creator,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Rewritten || out.Decision != "allow" || out.Outcome != "inline_task_approved" {
		t.Fatalf("explicit inline approval should resolve named inline hold; got %+v", out)
	}
	if !creator.called {
		t.Fatal("creator should be called for explicit inline approval")
	}
	if p, _ := cache.Peek(ctx, ResolveRequest{
		UserID: "user-1", AgentID: "agent-1", Provider: conversation.ProviderAnthropic,
		ApprovalID: inlineHeld.Pending.ID,
	}); p != nil {
		t.Fatalf("explicit inline hold should be consumed; got %+v", p)
	}
	if p, _ := cache.Peek(ctx, ResolveRequest{
		UserID: "user-1", AgentID: "agent-1", Provider: conversation.ProviderAnthropic,
		ApprovalID: toolHeld.Pending.ID,
	}); p == nil {
		t.Fatal("newer unrelated tool hold should remain")
	}
}

func TestApprovalRoutingCharacterization_TaskReplyDropsOnlyNamedToolHold(t *testing.T) {
	ctx := context.Background()
	cache := NewMemoryPendingApprovalCache(time.Minute)

	toolHeld, err := cache.Hold(ctx, PendingLiteApproval{
		ID:       "cv-cccccccccccccccccccccccccc",
		UserID:   "user-1",
		AgentID:  "agent-1",
		Provider: conversation.ProviderAnthropic,
		Stage:    StageTool,
		ToolUse:  conversation.ToolUse{ID: "toolu_named", Name: "Bash"},
	})
	if err != nil {
		t.Fatal(err)
	}
	inlineHeld, err := cache.Hold(ctx, PendingLiteApproval{
		ID:       "cv-dddddddddddddddddddddddddd",
		UserID:   "user-1",
		AgentID:  "agent-1",
		Provider: conversation.ProviderAnthropic,
		Stage:    StageAwaitingTaskApproval,
		ToolUse:  conversation.ToolUse{ID: "toolu_post", Name: "Bash"},
		TaskDefinition: &runtimetasks.TaskCreateRequest{
			Purpose: "Already proposed task",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	body := []byte(`{"messages":[{"role":"user","content":"task ` + toolHeld.Pending.ID + `"}]}`)
	out, err := RewriteTaskApprovalReply(ctx, TaskReplyRewriteRequest{
		HTTPRequest:     httptest.NewRequest("POST", "/v1/messages", nil),
		Provider:        conversation.ProviderAnthropic,
		Body:            body,
		Agent:           &store.Agent{ID: "agent-1", UserID: "user-1"},
		PendingApproval: cache,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Rewritten || !strings.Contains(string(out.Body), "surface=inline") {
		t.Fatalf("task reply should rewrite to inline task-creation prompt; got %+v body=%s", out, out.Body)
	}
	if p, _ := cache.Peek(ctx, ResolveRequest{
		UserID: "user-1", AgentID: "agent-1", Provider: conversation.ProviderAnthropic,
		ApprovalID: toolHeld.Pending.ID,
	}); p != nil {
		t.Fatalf("named tool hold should be consumed by task reply; got %+v", p)
	}
	if p, _ := cache.Peek(ctx, ResolveRequest{
		UserID: "user-1", AgentID: "agent-1", Provider: conversation.ProviderAnthropic,
		ApprovalID: inlineHeld.Pending.ID,
	}); p == nil {
		t.Fatal("unrelated newer inline hold should remain")
	}
}

func TestApprovalBodyReplacementProviderShapes(t *testing.T) {
	const replacement = "[replacement context]"

	cases := []struct {
		name       string
		provider   conversation.Provider
		path       string
		body       string
		want       string
		wantAbsent string
	}{
		{
			name:     "anthropic_string_content",
			provider: conversation.ProviderAnthropic,
			path:     "/v1/messages",
			body:     `{"messages":[{"role":"user","content":"approve"}]}`,
			want:     `"content":"` + replacement + `"`,
		},
		{
			name:     "anthropic_text_blocks",
			provider: conversation.ProviderAnthropic,
			path:     "/v1/messages",
			body: `{"messages":[{"role":"user","content":[` +
				`{"type":"text","text":"approve cv-abcdef123456"},` +
				`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"abc"}}]}]}`,
			want:       `"content":"` + replacement + `"`,
			wantAbsent: "cv-abcdef123456",
		},
		{
			name:     "openai_chat_string_content",
			provider: conversation.ProviderOpenAI,
			path:     "/v1/chat/completions",
			body:     `{"messages":[{"role":"user","content":"approve"}]}`,
			want:     `"content":"` + replacement + `"`,
		},
		{
			name:     "openai_responses_string_input",
			provider: conversation.ProviderOpenAI,
			path:     "/v1/responses",
			body:     `{"input":"approve"}`,
			want:     `"input":"` + replacement + `"`,
		},
		{
			name:     "openai_responses_message_blocks",
			provider: conversation.ProviderOpenAI,
			path:     "/v1/responses",
			body: `{"input":[{"type":"message","role":"user","content":[` +
				`{"type":"input_text","text":"approve cv-abcdef123456"}]}]}`,
			want:       `"text":"` + replacement + `"`,
			wantAbsent: "cv-abcdef123456",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", tc.path, nil)
			out, ok, err := replaceApprovalReplyForProvider(req, tc.provider, []byte(tc.body), "approve", replacement)
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
