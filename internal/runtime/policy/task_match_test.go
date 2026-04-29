package policy

import (
	"testing"

	"github.com/clawvisor/clawvisor/pkg/store"
)

func TestMatchToolCallAndEgressRequest(t *testing.T) {
	t.Parallel()

	task := &store.Task{
		ID:            "task-1",
		SchemaVersion: 2,
		ExpectedTools: []byte(`[
			{"tool_name":"fetch_messages","why":"triage inbox","input_shape":{"required_keys":["max_results"],"forbid_keys":["delete"]}}
		]`),
		ExpectedEgress: []byte(`[
			{"host":"api.example.com","why":"lookup records","method":"POST","path":"/v1/search","body_shape":{"required_keys":["query"],"forbid_keys":["admin"]}}
		]`),
	}

	toolMatch, err := MatchToolCall([]*store.Task{task}, "fetch_messages", map[string]any{"max_results": 10})
	if err != nil {
		t.Fatalf("MatchToolCall: %v", err)
	}
	if toolMatch == nil || toolMatch.TaskID != "task-1" {
		t.Fatalf("toolMatch=%+v", toolMatch)
	}
	toolMiss, err := MatchToolCall([]*store.Task{task}, "fetch_messages", map[string]any{"delete": true})
	if err != nil {
		t.Fatalf("MatchToolCall miss: %v", err)
	}
	if toolMiss != nil {
		t.Fatalf("expected tool miss, got %+v", toolMiss)
	}

	egressMatch, err := MatchEgressRequest([]*store.Task{task}, EgressRequest{
		Host:   "api.example.com",
		Method: "POST",
		Path:   "/v1/search",
		Body:   map[string]any{"query": "inbox"},
	})
	if err != nil {
		t.Fatalf("MatchEgressRequest: %v", err)
	}
	if egressMatch == nil || egressMatch.TaskID != "task-1" {
		t.Fatalf("egressMatch=%+v", egressMatch)
	}
	egressMiss, err := MatchEgressRequest([]*store.Task{task}, EgressRequest{
		Host:   "api.example.com",
		Method: "POST",
		Path:   "/v1/search",
		Body:   map[string]any{"query": "inbox", "admin": true},
	})
	if err != nil {
		t.Fatalf("MatchEgressRequest miss: %v", err)
	}
	if egressMiss != nil {
		t.Fatalf("expected egress miss, got %+v", egressMiss)
	}
}
