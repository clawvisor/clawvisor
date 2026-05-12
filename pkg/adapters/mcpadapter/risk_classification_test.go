package mcpadapter

import (
	"encoding/json"
	"testing"

	"github.com/clawvisor/clawvisor/pkg/adapters/mcpclient"
)

// TestRiskClassification_SpecDefaults locks in the MCP spec's annotation
// defaults: readOnlyHint defaults to false, destructiveHint defaults to true,
// so an unannotated tool MUST be classified as write/high. Anything weaker
// would silently let unannotated tools bypass approval/scope gates.
func TestRiskClassification_SpecDefaults(t *testing.T) {
	cases := []struct {
		name        string
		annotations map[string]any
		wantCat     string
		wantSens    string
	}{
		{
			// The spec default: no hints. Treat as writable + destructive
			// since that's the conservative reading of an absent annotation.
			name:        "no annotations defaults to write/high",
			annotations: nil,
			wantCat:     "write",
			wantSens:    "high",
		},
		{
			name:        "readOnlyHint:true downgrades to read/low",
			annotations: map[string]any{"readOnlyHint": true},
			wantCat:     "read",
			wantSens:    "low",
		},
		{
			name:        "destructiveHint:false downgrades to write/medium",
			annotations: map[string]any{"destructiveHint": false},
			wantCat:     "write",
			wantSens:    "medium",
		},
		{
			name:        "explicit destructiveHint:true keeps write/high",
			annotations: map[string]any{"destructiveHint": true},
			wantCat:     "write",
			wantSens:    "high",
		},
		{
			// A misconfigured server that asserts both; the safer claim
			// (readOnly) wins because it's a positive safety statement,
			// while destructive is the unknown default.
			name:        "readOnly:true beats destructive:true",
			annotations: map[string]any{"readOnlyHint": true, "destructiveHint": true},
			wantCat:     "read",
			wantSens:    "low",
		},
		{
			name:        "readOnlyHint:false (explicit) keeps write/high",
			annotations: map[string]any{"readOnlyHint": false},
			wantCat:     "write",
			wantSens:    "high",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			adapter := New(Config{ServiceID: "test"})
			adapter.tools = []mcpclient.Tool{
				{
					Name:        "tool",
					Description: "x.",
					InputSchema: json.RawMessage(`{}`),
					Annotations: tc.annotations,
				},
			}
			meta := adapter.ServiceMetadata().ActionMeta["tool"]
			if meta.Category != tc.wantCat || meta.Sensitivity != tc.wantSens {
				t.Errorf("annotations=%v: got %s/%s, want %s/%s",
					tc.annotations, meta.Category, meta.Sensitivity,
					tc.wantCat, tc.wantSens)
			}
		})
	}
}
