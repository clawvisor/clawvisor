package policies

import (
	"context"
	"net/http"
	"testing"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/orggov"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/pipeline"
)

// stubReq is a minimal pipeline.ReadOnlyRequest for testing the
// model policy. Only the fields the policy reads are populated.
type stubReq struct {
	provider conversation.Provider
	body     []byte
	userID   string
	agentID  string
}

func (s *stubReq) Provider() conversation.Provider                { return s.provider }
func (s *stubReq) StreamShape() conversation.StreamShape          { return 0 }
func (s *stubReq) Turns() []conversation.Turn                     { return nil }
func (s *stubReq) HTTPRequest() *http.Request                     { return nil }
func (s *stubReq) RawBody() []byte                                { return s.body }
func (s *stubReq) IsFirstTurn() bool                              { return true }
func (s *stubReq) ConversationID() string                         { return "" }
func (s *stubReq) UserID() string                                 { return s.userID }
func (s *stubReq) AgentID() string                                { return s.agentID }
func (s *stubReq) ValidateReplacementBody([]byte) error           { return nil }

type stubMut struct{}

func (stubMut) ReplaceBody([]byte) error                              { return nil }
func (stubMut) SetHeader(string, string)                              {}
func (stubMut) AppendContinuationTurn(pipeline.SyntheticContinuation) {}

func TestOrgModelPolicy_NoOpWhenNoCallbackOrNoOrg(t *testing.T) {
	// No callback wired.
	p := NewOrgModelPolicy(orggov.Callbacks{}, func(context.Context, string) string { return "org-a" })
	req := &stubReq{provider: conversation.ProviderAnthropic, body: []byte(`{"model":"claude-opus-4-7"}`), agentID: "a1"}
	v, err := p.Preprocess(context.Background(), req, nil)
	if err != nil || v.Outcome != pipeline.OutcomeAllow {
		t.Errorf("no-callback path: %+v err=%v", v, err)
	}

	// Callback wired but agent has no org.
	called := false
	p = NewOrgModelPolicy(orggov.Callbacks{CheckModelPolicy: func(context.Context, string, string) (bool, string) {
		called = true
		return false, "should not be reached"
	}}, func(context.Context, string) string { return "" })
	v, _ = p.Preprocess(context.Background(), req, nil)
	if called {
		t.Error("callback should not be called when orgID is empty")
	}
	if v.Outcome != pipeline.OutcomeAllow {
		t.Errorf("empty-org path expected Allow, got %v", v.Outcome)
	}
}

func TestOrgModelPolicy_CanonicalizesUsingProvider(t *testing.T) {
	var seen string
	p := NewOrgModelPolicy(orggov.Callbacks{
		CheckModelPolicy: func(_ context.Context, _, model string) (bool, string) {
			seen = model
			return true, ""
		},
	}, func(context.Context, string) string { return "org-a" })

	// Anthropic: bare "claude-opus-4-7" → "anthropic/claude-opus-4-7".
	req := &stubReq{provider: conversation.ProviderAnthropic, body: []byte(`{"model":"claude-opus-4-7"}`), agentID: "a1"}
	if _, err := p.Preprocess(context.Background(), req, nil); err != nil {
		t.Fatalf("preprocess: %v", err)
	}
	if seen != "anthropic/claude-opus-4-7" {
		t.Errorf("expected anthropic prefix, got %q", seen)
	}

	// OpenAI: bare "gpt-4o" → "openai/gpt-4o".
	req = &stubReq{provider: conversation.ProviderOpenAI, body: []byte(`{"model":"gpt-4o"}`), agentID: "a1"}
	if _, err := p.Preprocess(context.Background(), req, nil); err != nil {
		t.Fatalf("preprocess: %v", err)
	}
	if seen != "openai/gpt-4o" {
		t.Errorf("expected openai prefix, got %q", seen)
	}

	// Already-qualified passes through untouched.
	req = &stubReq{provider: conversation.ProviderOpenAI, body: []byte(`{"model":"azure/gpt-4o-2024-08"}`), agentID: "a1"}
	if _, err := p.Preprocess(context.Background(), req, nil); err != nil {
		t.Fatalf("preprocess: %v", err)
	}
	if seen != "azure/gpt-4o-2024-08" {
		t.Errorf("expected pass-through, got %q", seen)
	}
}

func TestOrgModelPolicy_BlockEmitsDenyAndRecordsViolation(t *testing.T) {
	recorded := false
	p := NewOrgModelPolicy(orggov.Callbacks{
		CheckModelPolicy: func(_ context.Context, _, _ string) (bool, string) {
			return false, "model blocked by org policy"
		},
		RecordViolation: func(_ context.Context, evt orggov.ViolationEvent) {
			recorded = true
			if evt.PolicyKind != "model_policy" || evt.ActionTaken != "blocked" {
				t.Errorf("violation event shape wrong: %+v", evt)
			}
			if evt.OrgID != "org-a" || evt.AgentID != "a1" {
				t.Errorf("violation identifiers wrong: %+v", evt)
			}
		},
	}, func(context.Context, string) string { return "org-a" })

	req := &stubReq{provider: conversation.ProviderAnthropic, body: []byte(`{"model":"claude-opus-4-7"}`), agentID: "a1", userID: "u1"}
	v, err := p.Preprocess(context.Background(), req, nil)
	if err != nil {
		t.Fatalf("preprocess: %v", err)
	}
	if v.Outcome != pipeline.OutcomeDeny {
		t.Errorf("expected deny, got %v", v.Outcome)
	}
	if !recorded {
		t.Error("RecordViolation was not called")
	}
	if v.Reason == "" {
		t.Error("expected non-empty reason")
	}
}

func TestOrgModelPolicy_EmptyModelNoOps(t *testing.T) {
	called := false
	p := NewOrgModelPolicy(orggov.Callbacks{CheckModelPolicy: func(context.Context, string, string) (bool, string) {
		called = true
		return false, "should not be called"
	}}, func(context.Context, string) string { return "org-a" })
	req := &stubReq{provider: conversation.ProviderAnthropic, body: []byte(`{}`), agentID: "a1"}
	v, _ := p.Preprocess(context.Background(), req, nil)
	if called {
		t.Error("callback called despite empty model")
	}
	if v.Outcome != pipeline.OutcomeAllow {
		t.Errorf("expected Allow for empty model, got %v", v.Outcome)
	}
}
