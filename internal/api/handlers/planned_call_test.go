package handlers

import (
	"testing"

	"github.com/clawvisor/clawvisor/pkg/store"
)

func TestMatchPlannedCall_RequiresSameParams(t *testing.T) {
	planned := []store.PlannedCall{{
		Service: "google.gmail", Action: "send_message", Reason: "Reply to Alice",
		Params: map[string]any{"to": "alice@example.com", "subject": "Re: lunch", "thread_id": "$chain"},
	}}
	facts := []store.ChainFact{{FactValue: "thread-1"}}
	base := func() map[string]any {
		return map[string]any{"to": "alice@example.com", "subject": "Re: lunch", "thread_id": "thread-1"}
	}

	cases := []struct {
		name   string
		mutate func(map[string]any)
		want   bool
	}{
		{name: "identical", mutate: func(map[string]any) {}, want: true},
		{name: "empty-string extra param", mutate: func(p map[string]any) { p["cc"] = "" }, want: false},
		{name: "nil extra param is ignored", mutate: func(p map[string]any) { p["cc"] = nil }, want: true},
		{name: "extra bcc", mutate: func(p map[string]any) { p["bcc"] = "attacker@evil.example" }, want: false},
		{name: "extra body", mutate: func(p map[string]any) { p["body"] = "anything" }, want: false},
		{name: "different recipient", mutate: func(p map[string]any) { p["to"] = "attacker@evil.example" }, want: false},
		{name: "missing planned param", mutate: func(p map[string]any) { delete(p, "subject") }, want: false},
		{name: "chain value not in facts", mutate: func(p map[string]any) { p["thread_id"] = "thread-2" }, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			params := base()
			tc.mutate(params)
			got := matchPlannedCall(planned, "google.gmail", "send_message", params, facts) != nil
			if got != tc.want {
				t.Fatalf("match = %v, want %v (params %v)", got, tc.want, params)
			}
		})
	}
}
