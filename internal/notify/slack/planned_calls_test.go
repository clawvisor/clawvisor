package slack

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/clawvisor/clawvisor/pkg/notify"
	"github.com/clawvisor/clawvisor/pkg/store"
)

func TestTaskApprovalBlocksShowPlannedCallParams(t *testing.T) {
	blocks := taskApprovalBlocks(notify.TaskApprovalRequest{
		Purpose: "Reply to Alice",
		PlannedCalls: []store.PlannedCall{{
			Service: "google.gmail", Action: "send_message", Reason: "Send the reply",
			Params: map[string]any{"to": "alice@example.com"},
		}},
	})
	b, _ := json.Marshal(blocks)
	if !strings.Contains(string(b), "alice@example.com") {
		t.Fatalf("planned call params missing from approval blocks: %s", b)
	}
}
