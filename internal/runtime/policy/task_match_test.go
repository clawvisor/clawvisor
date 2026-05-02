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

func TestMatchToolCallPrefersMoreSpecificCandidate(t *testing.T) {
	t.Parallel()

	broad := &store.Task{
		ID:            "task-broad",
		SchemaVersion: 2,
		ExpectedTools: []byte(`[
			{"tool_name":"fetch_messages","why":"generic triage"}
		]`),
	}
	specific := &store.Task{
		ID:            "task-specific",
		SchemaVersion: 2,
		ExpectedTools: []byte(`[
			{"tool_name":"fetch_messages","why":"narrow triage","input_shape":{"required_keys":["thread_id"]}}
		]`),
	}

	match, err := MatchToolCall([]*store.Task{broad, specific}, "fetch_messages", map[string]any{"thread_id": "thr_123"})
	if err != nil {
		t.Fatalf("MatchToolCall: %v", err)
	}
	if match == nil || match.TaskID != "task-specific" {
		t.Fatalf("expected specific task match, got %+v", match)
	}
}

func TestMatchEgressRequestPrefersMoreSpecificCandidate(t *testing.T) {
	t.Parallel()

	broad := &store.Task{
		ID:            "task-broad",
		SchemaVersion: 2,
		ExpectedEgress: []byte(`[
			{"host":"api.example.com","why":"generic runtime access","method":"GET","path_regex":"^/v1/.*$"}
		]`),
	}
	specific := &store.Task{
		ID:            "task-specific",
		SchemaVersion: 2,
		ExpectedEgress: []byte(`[
			{"host":"api.example.com","why":"read search endpoint","method":"GET","path":"/v1/search"}
		]`),
	}

	match, err := MatchEgressRequest([]*store.Task{broad, specific}, EgressRequest{
		Host:   "api.example.com",
		Method: "GET",
		Path:   "/v1/search",
	})
	if err != nil {
		t.Fatalf("MatchEgressRequest: %v", err)
	}
	if match == nil || match.TaskID != "task-specific" {
		t.Fatalf("expected specific task match, got %+v", match)
	}
}

func TestMatchRegexMapUsesFlattenedStructuredRepresentation(t *testing.T) {
	t.Parallel()

	input := map[string]any{
		"url": "https://example.com",
		"options": map[string]any{
			"extractMode": "text",
		},
	}
	ok, err := matchRegexMap(`(?m)^url="https://example\.com"$`, input)
	if err != nil {
		t.Fatalf("matchRegexMap: %v", err)
	}
	if !ok {
		t.Fatal("expected flattened key=value regex to match")
	}
	ok, err = matchRegexMap(`(?m)^options\.extractMode="text"$`, input)
	if err != nil {
		t.Fatalf("matchRegexMap nested: %v", err)
	}
	if !ok {
		t.Fatal("expected nested flattened key=value regex to match")
	}
}
