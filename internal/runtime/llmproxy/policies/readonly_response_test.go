package policies_test

import (
	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/pipeline"
)

// stubReadOnlyResponse is a minimal ReadOnlyResponse for policy tests.
type stubReadOnlyResponse struct {
	provider  conversation.Provider
	shape     conversation.StreamShape
	streaming bool
	toolUses  []conversation.ToolUse
}

func (s *stubReadOnlyResponse) Provider() conversation.Provider       { return s.provider }
func (s *stubReadOnlyResponse) StreamShape() conversation.StreamShape { return s.shape }
func (s *stubReadOnlyResponse) IsStreaming() bool                     { return s.streaming }
func (s *stubReadOnlyResponse) ToolUses() []conversation.ToolUse      { return s.toolUses }

var _ pipeline.ReadOnlyResponse = (*stubReadOnlyResponse)(nil)
