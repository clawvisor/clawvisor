package policies_test

import (
	"encoding/json"
	"net/http"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/pipeline"
)

// recordingRequestMutator captures mutation intent for assertions.
// Used by policy tests that want to verify which mutator methods a
// policy called and with what arguments — without exercising the
// real (provider-aware) mutator implementation.
type recordingRequestMutator struct {
	ReplaceBodyCalls         [][]byte
	InjectSystemNoticeCalls  []string
	PrependUserTurnCalls     []string
	RewriteHistoricalCalls   []rewriteHistoricalCall
	StripTurnsCalls          int
	RewriteUserTextCalls     []string
	RedactSpansCalls         [][]pipeline.ByteSpan
	AppendContinuationCalls  []pipeline.SyntheticContinuation
}

type rewriteHistoricalCall struct {
	ToolUseID string
	NewInput  json.RawMessage
}

func (m *recordingRequestMutator) ReplaceBody(b []byte) error {
	m.ReplaceBodyCalls = append(m.ReplaceBodyCalls, append([]byte(nil), b...))
	return nil
}
func (m *recordingRequestMutator) InjectSystemNotice(t string) error {
	m.InjectSystemNoticeCalls = append(m.InjectSystemNoticeCalls, t)
	return nil
}
func (m *recordingRequestMutator) PrependUserTurn(t string) error {
	m.PrependUserTurnCalls = append(m.PrependUserTurnCalls, t)
	return nil
}
func (m *recordingRequestMutator) RewriteHistoricalToolUseArgs(id string, in json.RawMessage) error {
	m.RewriteHistoricalCalls = append(m.RewriteHistoricalCalls, rewriteHistoricalCall{ToolUseID: id, NewInput: append(json.RawMessage(nil), in...)})
	return nil
}
func (m *recordingRequestMutator) StripTurns(_ func(pipeline.StripContext) bool) error {
	m.StripTurnsCalls++
	return nil
}
func (m *recordingRequestMutator) RewriteMostRecentUserText(t string) error {
	m.RewriteUserTextCalls = append(m.RewriteUserTextCalls, t)
	return nil
}
func (m *recordingRequestMutator) RedactSpans(spans []pipeline.ByteSpan) error {
	cp := make([]pipeline.ByteSpan, len(spans))
	copy(cp, spans)
	m.RedactSpansCalls = append(m.RedactSpansCalls, cp)
	return nil
}
func (m *recordingRequestMutator) AppendContinuationTurn(s pipeline.SyntheticContinuation) error {
	m.AppendContinuationCalls = append(m.AppendContinuationCalls, s)
	return nil
}

var _ pipeline.RequestMutator = (*recordingRequestMutator)(nil)

// stubReadOnlyRequest is a no-frills ReadOnlyRequest backed by simple
// fields. Used in policy tests where the policy only needs to know
// the provider and the raw body.
type stubReadOnlyRequest struct {
	provider  conversation.Provider
	shape     conversation.StreamShape
	rawBody   []byte
	firstTurn bool
	convID    string
	userID    string
	agentID   string
}

func (s *stubReadOnlyRequest) Provider() conversation.Provider       { return s.provider }
func (s *stubReadOnlyRequest) StreamShape() conversation.StreamShape { return s.shape }
func (s *stubReadOnlyRequest) Turns() []conversation.Turn            { return nil }
func (s *stubReadOnlyRequest) HTTPRequest() *http.Request            { return nil }
func (s *stubReadOnlyRequest) RawBody() []byte                       { return s.rawBody }
func (s *stubReadOnlyRequest) IsFirstTurn() bool                     { return s.firstTurn }
func (s *stubReadOnlyRequest) ConversationID() string                { return s.convID }
func (s *stubReadOnlyRequest) UserID() string                        { return s.userID }
func (s *stubReadOnlyRequest) AgentID() string                       { return s.agentID }

var _ pipeline.ReadOnlyRequest = (*stubReadOnlyRequest)(nil)
