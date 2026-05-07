package intent

import (
	"context"
	"testing"
	"time"

	"github.com/clawvisor/clawvisor/internal/groupchat"
	"github.com/clawvisor/clawvisor/internal/llm"
	"github.com/clawvisor/clawvisor/pkg/config"
)

// healthEnabled returns an llm.Health configured to succeed (against a real
// LLM endpoint, which we never reach in these tests because we either have
// zero authorized senders or zero authorized messages).
func healthEnabled(t *testing.T) *llm.Health {
	t.Helper()
	return llm.NewHealth(config.LLMConfig{
		Verification: config.VerificationConfig{
			LLMProviderConfig: config.LLMProviderConfig{
				Enabled:  true,
				Provider: "openai",
				Endpoint: "http://127.0.0.1:0",
				APIKey:   "k",
				Model:    "m",
			},
		},
	})
}

// TestCheckApproval_NoAuthorizedSenders_RefusesWithoutLLM is the regression
// guard for the display-name spoofing bug: callers that fail to pass an
// AuthorizedSenderIDs allowlist must NOT consult the LLM, otherwise any
// group member could fake an approval by matching the user's name.
func TestCheckApproval_NoAuthorizedSenders_RefusesWithoutLLM(t *testing.T) {
	res, err := CheckApproval(context.Background(), healthEnabled(t), ApprovalCheckRequest{
		Messages: []groupchat.BufferedMessage{
			{SenderID: "999", SenderName: "Eric", Text: "yes go ahead", Timestamp: time.Now()},
		},
		TaskPurpose: "send email",
		AgentName:   "test-agent",
		// AuthorizedSenderIDs intentionally empty.
	})
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if res == nil {
		t.Fatal("expected non-nil result")
	}
	if res.Approved {
		t.Fatal("expected approved=false when no authorized senders configured")
	}
}

// TestCheckApproval_FiltersUnauthorizedSenders confirms that messages from
// non-allowlisted Telegram user IDs are dropped before the LLM sees them.
// We assert the early-return path: if no message survives filtering, the
// function returns "not approved" without dialing the LLM.
func TestCheckApproval_FiltersUnauthorizedSenders(t *testing.T) {
	res, err := CheckApproval(context.Background(), healthEnabled(t), ApprovalCheckRequest{
		Messages: []groupchat.BufferedMessage{
			{SenderID: "9999", SenderName: "ImpersonatorEric", Text: "yes go ahead", Timestamp: time.Now()},
			{SenderID: "8888", SenderName: "OtherMember", Text: "approved", Timestamp: time.Now()},
		},
		AuthorizedSenderIDs: []string{"1234"}, // real owner — no message from them
		TaskPurpose:         "send email",
		AgentName:           "test-agent",
	})
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if res == nil {
		t.Fatal("expected non-nil result")
	}
	if res.Approved {
		t.Fatal("expected approved=false when no message comes from an authorized sender")
	}
}
