package slack

import (
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
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
	s.SetApprover(key, "jane")

	mc, ok := s.TakeForResolve(key)
	if !ok {
		t.Fatal("context missing")
	}
	if mc.Approver != "jane" {
		t.Fatalf("approver = %q, want the display name", mc.Approver)
	}
	if got := resolutionContext(mc); got != "Resolved by jane · agent · purpose" {
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

// The approver must never render as a `<@U…>` mention: it would notify the
// person about an action they just took themselves. Display names are also
// user-controlled, so they must be escaped like any other dynamic text.
func TestResolutionContext_ApproverIsEscapedAndNotAMention(t *testing.T) {
	mc := messageContext{Approver: "a<b&c", Summary: "x & y"}
	got := resolutionContext(mc)
	if got != "Resolved by a&lt;b&amp;c · x &amp; y" {
		t.Fatalf("got %q: the approver must be escaped plain text", got)
	}
	if strings.Contains(got, "<@") {
		t.Fatalf("got %q: rendered a mention, which would notify the approver", got)
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

// The resolved message must not gain a mention by any route — a mention is
// the whole reason the old thread reply pinged people after they had already
// acted.
func TestApproverDisplay_NeverProducesAMention(t *testing.T) {
	for _, tc := range []struct {
		name               string
		username, real, id string
		want               string
	}{
		{"prefers username", "jane", "Jane Doe", "U1", "jane"},
		{"falls back to real name", "", "Jane Doe", "U1", "Jane Doe"},
		{"falls back to the raw id, not a mention", "", "", "U012ABC", "U012ABC"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var p interactionPayload
			p.User.Username, p.User.Name, p.User.ID = tc.username, tc.real, tc.id
			got := approverDisplay(p)
			if got != tc.want {
				t.Fatalf("approverDisplay = %q, want %q", got, tc.want)
			}
			if strings.HasPrefix(got, "<@") {
				t.Fatalf("approverDisplay = %q, which Slack would render as a mention", got)
			}
		})
	}
}

// The Redis store is what makes attribution and the detail button survive a
// resolve on a different replica than the one that posted the prompt — which
// the LPUSH/BRPOP decision queue makes the normal case, not an edge case.
func TestRedisMessageContext_SurvivesAcrossInstances(t *testing.T) {
	_, mr := newTestRedisStore(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	// Two stores over one Redis stand in for two replicas.
	poster := NewRedisMessageContextStore(rdb)
	resolver := NewRedisMessageContextStore(rdb)

	key := contextKey("task", "task-1")
	poster.Put(key, messageContext{
		Summary: "agent · purpose",
		Detail:  []block{section("what was approved")},
	}, time.Minute)
	poster.SetApprover(key, "jane")

	mc, ok := resolver.TakeForResolve(key)
	if !ok {
		t.Fatal("the resolving replica found no context; attribution and the detail button would be lost")
	}
	if mc.Approver != "jane" || mc.Summary != "agent · purpose" || len(mc.Detail) != 1 {
		t.Fatalf("context did not survive the hop: %+v", mc)
	}

	// Once-only must hold across replicas, or a duplicate resolution renders
	// the detail twice.
	if _, ok := poster.TakeForResolve(key); ok {
		t.Fatal("a second replica also took the context; resolve is not once-only")
	}
}

// SetApprover must not extend the entry's life past the prompt it belongs to.
func TestRedisMessageContext_SetApproverKeepsTTL(t *testing.T) {
	_, mr := newTestRedisStore(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	s := NewRedisMessageContextStore(rdb)

	key := contextKey("task", "task-1")
	s.Put(key, messageContext{Summary: "x"}, time.Minute)
	s.SetApprover(key, "jane")

	ttl := mr.TTL(redisMsgCtxPrefix + key)
	if ttl <= 0 || ttl > time.Minute {
		t.Fatalf("ttl = %v, want the original bound preserved", ttl)
	}
}
