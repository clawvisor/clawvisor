package notify

import (
	"strings"
	"testing"

	"github.com/clawvisor/clawvisor/pkg/store"
)

func TestRenderPlannedCallParams(t *testing.T) {
	if got := RenderPlannedCallParams(store.PlannedCall{}); got != "" {
		t.Fatalf("no params: got %q, want empty", got)
	}

	got := RenderPlannedCallParams(store.PlannedCall{Params: map[string]any{"to": "alice@example.com", "thread_id": "$chain"}})
	if want := `{"thread_id":"$chain","to":"alice@example.com"}`; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}

	long := RenderPlannedCallParams(store.PlannedCall{Params: map[string]any{"body": strings.Repeat("x", 2000)}})
	if !strings.Contains(long, "truncated") {
		t.Fatalf("long params should carry a truncation marker, got %d chars without one", len(long))
	}
	if n := len([]rune(long)); n != plannedParamsMaxLen {
		t.Fatalf("long params rendered at %d runes, want exactly %d", n, plannedParamsMaxLen)
	}
}
