package notify

import (
	"strings"
	"testing"
)

// TestRenderExpansionSummary_NoEntriesSentinel ensures the renderer
// returns a non-empty sentinel for an empty expansion so consumers
// (push action_summary, live-activity status) don't render blank.
func TestRenderExpansionSummary_NoEntriesSentinel(t *testing.T) {
	if got := RenderExpansionSummary(ScopeExpansionRequest{}); got != "scope_expansion" {
		t.Errorf("RenderExpansionSummary(empty) = %q, want sentinel", got)
	}
}

// TestRenderExpansionSummary_TruncatesWithMoreSuffix checks the
// elision path: many small entries should cap at ~256 bytes and
// emit a "(+N more)" suffix rather than letting the join run away.
func TestRenderExpansionSummary_TruncatesWithMoreSuffix(t *testing.T) {
	var tools []ExpansionTool
	for i := 0; i < 50; i++ {
		tools = append(tools, ExpansionTool{ToolName: "service.tool_" + strings.Repeat("x", 8)})
	}
	out := RenderExpansionSummary(ScopeExpansionRequest{AddedTools: tools})
	if len(out) > scopeExpansionMaxCompactLen {
		// joinWithCap now reserves the suffix length against the cap;
		// final output must stay at or below maxBytes, not just within
		// "maxBytes + suffix slack".
		t.Errorf("output len = %d exceeds cap %d (suffix should be reserved): %q", len(out), scopeExpansionMaxCompactLen, out)
	}
	if !strings.Contains(out, "more)") {
		t.Errorf("output missing 'more)' suffix; truncation did not fire: %q", out)
	}
}

// TestJoinWithCap_FullJoinFitsReturnedVerbatim guards against the
// premature-truncation regression: a small cap that nevertheless
// fits the full join must NOT trigger the bail-suffix path. The
// earlier reserve-suffix-per-iteration approach over-reserved and
// truncated "abc, def" (8 bytes) inside a 20-byte cap.
func TestJoinWithCap_FullJoinFitsReturnedVerbatim(t *testing.T) {
	out := joinWithCap([]string{"abc", "def"}, ", ", 20)
	if out != "abc, def" {
		t.Errorf("expected full join %q, got %q (premature truncation regressed)", "abc, def", out)
	}
}

// TestJoinWithCap_NearFullFirstItemThenBail covers the bug where
// the first item nearly fills the buffer and the second item's bail
// appends "(+N more)" without checking suffix room — pushing the
// total past maxBytes. The reservation-against-(maxBytes - suffix)
// fix bails at item 1 BEFORE writing it so the suffix lands inside
// the cap.
func TestJoinWithCap_NearFullFirstItemThenBail(t *testing.T) {
	cap := 20
	out := joinWithCap([]string{strings.Repeat("a", 15), strings.Repeat("b", 20)}, ", ", cap)
	if len(out) > cap {
		t.Errorf("output len = %d exceeds cap %d: %q", len(out), cap, out)
	}
}

// TestRenderExpansionSummary_SingleOversizedItemIsEllipsized is the
// regression test for the joinWithCap i==0 bug: a single tool with
// a name longer than scopeExpansionMaxCompactLen used to bypass the
// cap entirely because the truncation branch was gated on i > 0.
// Now we expect the first entry to be truncated in place with a "..."
// suffix and the total length to stay within the cap.
func TestRenderExpansionSummary_SingleOversizedItemIsEllipsized(t *testing.T) {
	huge := strings.Repeat("a", scopeExpansionMaxCompactLen+200)
	out := RenderExpansionSummary(ScopeExpansionRequest{
		AddedTools: []ExpansionTool{{ToolName: huge}},
	})
	if len(out) > scopeExpansionMaxCompactLen {
		t.Errorf("single oversized item bypassed cap: len=%d, want <= %d", len(out), scopeExpansionMaxCompactLen)
	}
	if !strings.HasSuffix(out, "...") {
		t.Errorf("single oversized item not ellipsized: %q", out)
	}
}

// TestCapExpansionBody_ShortPassThrough confirms a short body is
// returned verbatim — no needless allocation or formatting drift.
func TestCapExpansionBody_ShortPassThrough(t *testing.T) {
	in := "agent wants to expand scope: small reason"
	if got := CapExpansionBody(in); got != in {
		t.Errorf("CapExpansionBody(short) modified the body: got %q want %q", got, in)
	}
}

// TestCapExpansionBody_LongTruncates exercises the body cap so a
// runaway-model reason doesn't push the surrounding push payload
// past APNs/FCM size limits.
func TestCapExpansionBody_LongTruncates(t *testing.T) {
	long := strings.Repeat("x", scopeExpansionMaxBodyLen+100)
	got := CapExpansionBody(long)
	if len(got) > scopeExpansionMaxBodyLen {
		t.Errorf("body not capped: len=%d, want <= %d", len(got), scopeExpansionMaxBodyLen)
	}
	if !strings.HasSuffix(got, "...") {
		t.Errorf("truncated body missing ellipsis: %q", got[len(got)-10:])
	}
}
