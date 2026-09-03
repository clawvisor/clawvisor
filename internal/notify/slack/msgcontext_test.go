package slack

import (
	"testing"
	"time"
)

func TestMessageContext_ResolveIsOnceOnly(t *testing.T) {
	s := newMessageContextStore()
	key := contextKey("approval", "req-1")
	s.Put(key, messageContext{Summary: "agent · gmail:send", Detail: []block{section("x")}}, time.Minute)

	mc, ok := s.TakeForResolve(key)
	if !ok {
		t.Fatal("first resolve did not find the context")
	}
	if mc.Summary != "agent · gmail:send" || len(mc.Detail) != 1 {
		t.Fatalf("context did not round-trip: %+v", mc)
	}

	// A duplicate resolution must not post the detail thread a second time.
	if _, ok := s.TakeForResolve(key); ok {
		t.Fatal("second resolve returned a context; detail thread would be posted twice")
	}
}

func TestMessageContext_ApproverIsRecordedForResolve(t *testing.T) {
	s := newMessageContextStore()
	key := contextKey("task", "task-1")
	s.Put(key, messageContext{Summary: "agent · purpose"}, time.Minute)
	s.SetApprover(key, mention("U012ABC"))

	mc, ok := s.TakeForResolve(key)
	if !ok {
		t.Fatal("context missing")
	}
	if mc.Approver != "<@U012ABC>" {
		t.Fatalf("approver = %q, want a Slack mention", mc.Approver)
	}
	if got := resolutionContext(mc); got != "Resolved by <@U012ABC> · agent · purpose" {
		t.Fatalf("resolutionContext = %q", got)
	}
}

// Resolution from the dashboard or by expiry has no Slack clicker; the
// message must omit attribution rather than inventing one.
func TestResolutionContext_OmitsUnknownApprover(t *testing.T) {
	if got := resolutionContext(messageContext{Summary: "agent · gmail:send"}); got != "agent · gmail:send" {
		t.Fatalf("got %q, want the summary alone", got)
	}
	if got := resolutionContext(messageContext{}); got != "" {
		t.Fatalf("got %q, want an empty context line", got)
	}
}

// The mention must survive un-escaped or Slack renders "<@U012ABC>" as text
// instead of resolving it to the member's name.
func TestResolutionContext_MentionIsNotEscaped(t *testing.T) {
	mc := messageContext{Approver: mention("U012ABC"), Summary: "a & b"}
	got := resolutionContext(mc)
	if got != "Resolved by <@U012ABC> · a &amp; b" {
		t.Fatalf("got %q: mention must stay raw while the summary is escaped", got)
	}
}

func TestMessageContext_ExpiredEntryIsNotResolved(t *testing.T) {
	s := newMessageContextStore()
	key := contextKey("approval", "req-1")
	s.Put(key, messageContext{Summary: "x"}, -time.Second)
	if _, ok := s.TakeForResolve(key); ok {
		t.Fatal("expired context was returned")
	}
}

func TestTargetTypeForDecision(t *testing.T) {
	for in, want := range map[string]string{
		"approval": "approval",
		"task":     "task",
		// Scope expansion resolves against its parent task, so it must key
		// into the same namespace or the resolve lookup misses.
		"scope_expansion": "task",
		"connection":      "connection",
	} {
		if got := targetTypeForDecision(in); got != want {
			t.Fatalf("targetTypeForDecision(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSummarise_SkipsEmptyParts(t *testing.T) {
	if got := summarise("agent", "", "  ", "gmail:send"); got != "agent · gmail:send" {
		t.Fatalf("got %q", got)
	}
	if got := summarise("", ""); got != "" {
		t.Fatalf("got %q, want empty", got)
	}
}
