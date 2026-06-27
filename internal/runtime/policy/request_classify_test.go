package policy

import (
	"testing"

	"github.com/clawvisor/clawvisor/pkg/store"
)

// TestClassifyGatewayRequestPreferredIsStrict guards the
// per-conversation isolation invariant for the gateway-style
// classifier: when a checked-out task is supplied via preferredTaskID
// and that task does NOT cover the requested action, the result must
// be ClassificationNeedsNewTask (so the menu UI can offer a switch)
// rather than a silent ClassificationBelongsToExistingTask against a
// sibling task that happened to cover the action.
func TestClassifyGatewayRequestPreferredIsStrict(t *testing.T) {
	t.Parallel()

	preferred := &store.Task{
		ID:      "task-preferred",
		AgentID: "agent-1",
		Status:  "active",
		AuthorizedActions: []store.TaskAction{
			{Service: "gmail", Action: "send_message"},
		},
	}
	sibling1 := &store.Task{
		ID:      "task-sibling-1",
		AgentID: "agent-1",
		Status:  "active",
		AuthorizedActions: []store.TaskAction{
			{Service: "gmail", Action: "fetch_messages"},
		},
	}
	sibling2 := &store.Task{
		ID:      "task-sibling-2",
		AgentID: "agent-1",
		Status:  "active",
		AuthorizedActions: []store.TaskAction{
			{Service: "gmail", Action: "fetch_messages"},
		},
	}
	tasks := []*store.Task{preferred, sibling1, sibling2}

	t.Run("preferred covers action -> belongs_to_existing_task=preferred", func(t *testing.T) {
		got := ClassifyGatewayRequestPreferred(tasks, "agent-1", "gmail", "", "send_message", "task-preferred")
		if got.Kind != ClassificationBelongsToExistingTask {
			t.Fatalf("kind=%q, want %q", got.Kind, ClassificationBelongsToExistingTask)
		}
		if got.MatchedTask == nil || got.MatchedTask.ID != "task-preferred" {
			t.Fatalf("matched task=%+v, want task-preferred", got.MatchedTask)
		}
	})

	t.Run("preferred does NOT cover, siblings WOULD -> needs_new_task (no leak)", func(t *testing.T) {
		got := ClassifyGatewayRequestPreferred(tasks, "agent-1", "gmail", "", "fetch_messages", "task-preferred")
		if got.Kind != ClassificationNeedsNewTask {
			t.Fatalf("kind=%q, want %q — preferred-strict regression", got.Kind, ClassificationNeedsNewTask)
		}
		if got.MatchedTask != nil {
			t.Fatalf("matched task should be nil under preferred-strict, got %+v", got.MatchedTask)
		}
		if len(got.CandidateTasks) == 0 {
			t.Fatalf("expected candidate tasks surfaced for switch-task menu, got 0")
		}
	})

	t.Run("no preferred id -> full pool match (ambiguous when two siblings cover)", func(t *testing.T) {
		got := ClassifyGatewayRequestPreferred(tasks, "agent-1", "gmail", "", "fetch_messages", "")
		if got.Kind != ClassificationAmbiguous {
			t.Fatalf("kind=%q, want %q", got.Kind, ClassificationAmbiguous)
		}
		if len(got.CandidateTasks) != 2 {
			t.Fatalf("candidates=%d, want 2", len(got.CandidateTasks))
		}
	})

	t.Run("stale preferred id (no active task with that id) -> needs_new_task, NOT silent sibling match", func(t *testing.T) {
		// Preferred id doesn't point at any active task — e.g. the
		// checked-out task expired mid-conversation. The pre-fix
		// behavior here was to fall through to the full-pool match,
		// which would silently authorize the call against an unrelated
		// sibling task and is exactly the cross-conversation leak the
		// per-conversation isolation invariant exists to prevent.
		// Strict mode: return NeedsNewTask so the agent must explicitly
		// re-checkout (or have the proxy mint a fresh task) rather
		// than implicitly switching authorization target.
		oneSibling := []*store.Task{sibling1}
		got := ClassifyGatewayRequestPreferred(oneSibling, "agent-1", "gmail", "", "fetch_messages", "task-vanished")
		if got.Kind != ClassificationNeedsNewTask {
			t.Fatalf("kind=%q, want %q (stale preferred must not silently match a sibling)", got.Kind, ClassificationNeedsNewTask)
		}
		if got.MatchedTask != nil {
			t.Fatalf("matched task should be nil under stale-preferred strict mode, got %+v", got.MatchedTask)
		}
	})
}
