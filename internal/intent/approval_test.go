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

// TestFilterMessagesByAuthorizedSenders is the positive proof that the
// filter actually drops spoofers and keeps the owner — independent of the
// surrounding LLM call. The negative tests below pass if the LLM is
// unreachable too, so they don't prove the filter ran. This one does.
func TestFilterMessagesByAuthorizedSenders(t *testing.T) {
	msgs := []groupchat.BufferedMessage{
		{SenderID: "9999", SenderName: "Spoofer", Text: "yes go ahead", Timestamp: time.Now()},
		{SenderID: "1234", SenderName: "Owner", Text: "looks good", Timestamp: time.Now()},
		{SenderID: "8888", SenderName: "Other", Text: "approved", Timestamp: time.Now()},
		{SenderID: "1234", SenderName: "Owner", Text: "yes", Timestamp: time.Now()},
	}

	t.Run("keeps only owner messages", func(t *testing.T) {
		got := filterMessagesByAuthorizedSenders(msgs, []string{"1234"})
		if len(got) != 2 {
			t.Fatalf("expected 2 owner messages, got %d", len(got))
		}
		for _, m := range got {
			if m.SenderID != "1234" {
				t.Fatalf("filter leaked sender %q", m.SenderID)
			}
		}
	})

	t.Run("empty allowlist returns empty", func(t *testing.T) {
		if got := filterMessagesByAuthorizedSenders(msgs, nil); len(got) != 0 {
			t.Fatalf("expected empty result for nil allowlist, got %d", len(got))
		}
	})

	t.Run("multiple authorized IDs", func(t *testing.T) {
		got := filterMessagesByAuthorizedSenders(msgs, []string{"1234", "8888"})
		if len(got) != 3 {
			t.Fatalf("expected 3 messages from two authorized senders, got %d", len(got))
		}
	})

	t.Run("does not mutate input", func(t *testing.T) {
		// Filter must NOT alias the caller's backing array. Order matters:
		// if the implementation aliases via msgs[:0:len(msgs)] then loops
		// + appends, a non-matching FIRST element followed by a matching
		// SECOND would slot the matching message into orig[0], overwriting
		// the spoofer. After the call, orig[0] must still be the spoofer.
		orig := []groupchat.BufferedMessage{
			{SenderID: "9999", SenderName: "Spoofer", Text: "yes go ahead"},
			{SenderID: "1234", SenderName: "Owner", Text: "approved"},
		}
		_ = filterMessagesByAuthorizedSenders(orig, []string{"1234"})
		if orig[0].SenderID != "9999" {
			t.Fatalf("filter aliased + mutated caller slice: orig[0].SenderID=%q (want %q)", orig[0].SenderID, "9999")
		}
		if orig[1].SenderID != "1234" {
			t.Fatalf("filter mutated caller slice unexpectedly: orig[1].SenderID=%q (want %q)", orig[1].SenderID, "1234")
		}
	})
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
