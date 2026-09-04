package slack

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestDetailStore_RoundTripsAndExpires(t *testing.T) {
	s := newMemoryDetailStore()
	ctx := context.Background()
	entry := DetailEntry{UserID: "u1", TeamID: "T1", Blocks: []block{section("detail")}}

	if err := s.PutDetail(ctx, "tok", entry, time.Minute); err != nil {
		t.Fatal(err)
	}
	got, ok := s.GetDetail(ctx, "tok")
	if !ok || got.UserID != "u1" || got.TeamID != "T1" || len(got.Blocks) != 1 {
		t.Fatalf("entry did not round-trip: %+v ok=%v", got, ok)
	}

	if _, ok := s.GetDetail(ctx, "never-issued"); ok {
		t.Fatal("unknown token returned an entry")
	}

	if err := s.PutDetail(ctx, "old", entry, -time.Second); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.GetDetail(ctx, "old"); ok {
		t.Fatal("expired entry was returned")
	}
}

func TestDetailStore_CleanupDropsExpiredOnly(t *testing.T) {
	s := newMemoryDetailStore()
	ctx := context.Background()
	e := DetailEntry{UserID: "u1", Blocks: []block{section("x")}}
	_ = s.PutDetail(ctx, "live", e, time.Minute)
	_ = s.PutDetail(ctx, "dead", e, -time.Second)

	s.Cleanup()

	if _, ok := s.GetDetail(ctx, "live"); !ok {
		t.Fatal("cleanup dropped a live entry")
	}
	if _, ok := s.GetDetail(ctx, "dead"); ok {
		t.Fatal("cleanup kept an expired entry")
	}
}

// The modal caps at 100 blocks; exceeding it makes views.open reject the
// whole view, which would present as a button that silently does nothing.
func TestDetailModal_ClampsToSlackLimit(t *testing.T) {
	many := make([]block, 150)
	for i := range many {
		many[i] = section("x")
	}
	v := detailModal(many)
	blocks, _ := v["blocks"].([]block)
	if len(blocks) > 100 {
		t.Fatalf("modal has %d blocks, over Slack's limit of 100", len(blocks))
	}

	title, _ := v["title"].(map[string]any)
	if txt, _ := title["text"].(string); len(txt) > 24 {
		t.Fatalf("modal title %q exceeds Slack's 24-character limit", txt)
	}
}

// The button must carry an opaque token, never the target ID — otherwise
// anyone able to forge an interaction could enumerate other requests.
func TestViewDetailsAction_CarriesOpaqueToken(t *testing.T) {
	b := viewDetailsAction("deadbeefdeadbeefdeadbeefdeadbeef")
	elems, _ := b["elements"].([]any)
	if len(elems) != 1 {
		t.Fatalf("expected one button, got %d", len(elems))
	}
	btn, _ := elems[0].(map[string]any)
	if btn["action_id"] != actionViewDetails {
		t.Fatalf("action_id = %v", btn["action_id"])
	}
	val, _ := btn["value"].(string)
	if strings.Contains(val, ":") || strings.Contains(val, "|") {
		t.Fatalf("value %q looks like a target key rather than an opaque token", val)
	}
}
