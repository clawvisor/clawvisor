package policies_test

import (
	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/pipeline"
)

// recordingResponseMutator captures ResponseMutator command intent for
// assertions in policy tests. Like recordingRequestMutator, this lets
// tests verify which mutator method a policy called and with what args
// — without exercising the real (stream-codec-backed) implementation.
type recordingResponseMutator struct {
	PrependAssistantTextCalls     []string
	SubstituteEntireResponseCalls []string
}

func (m *recordingResponseMutator) PrependAssistantText(text string) error {
	m.PrependAssistantTextCalls = append(m.PrependAssistantTextCalls, text)
	return nil
}
func (m *recordingResponseMutator) SubstituteEntireResponse(text string) error {
	m.SubstituteEntireResponseCalls = append(m.SubstituteEntireResponseCalls, text)
	return nil
}
func (m *recordingResponseMutator) Commit() error { return nil }

var _ pipeline.ResponseMutator = (*recordingResponseMutator)(nil)

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
