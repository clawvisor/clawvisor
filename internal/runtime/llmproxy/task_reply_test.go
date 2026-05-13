package llmproxy

import (
	"encoding/json"
	"strings"
	"testing"
)

// flattenOpenAITaskReplyContent must scan all text-bearing blocks, not
// just the last one. A multi-block user message with the approve verb
// in any block — or split across blocks — was producing false negatives.
func TestFlattenOpenAITaskReplyContent_MultiBlock(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		mustHas string
	}{
		{
			name:    "approve_verb_in_first_block",
			raw:     `[{"type":"input_text","text":"approve cv-abc123"},{"type":"input_text","text":"trailing prose"}]`,
			mustHas: "approve cv-abc123",
		},
		{
			name:    "approve_split_across_blocks",
			raw:     `[{"type":"input_text","text":"please "},{"type":"input_text","text":"approve cv-xyz"}]`,
			mustHas: "approve cv-xyz",
		},
		{
			name:    "approve_in_middle",
			raw:     `[{"type":"input_text","text":"hi"},{"type":"input_text","text":"approve cv-mid"},{"type":"input_text","text":"thanks"}]`,
			mustHas: "approve cv-mid",
		},
		{
			name:    "simple_string",
			raw:     `"approve cv-simple"`,
			mustHas: "approve cv-simple",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := flattenOpenAITaskReplyContent(json.RawMessage(tc.raw))
			if !strings.Contains(got, tc.mustHas) {
				t.Fatalf("flattened content missing %q; got %q", tc.mustHas, got)
			}
		})
	}
}
