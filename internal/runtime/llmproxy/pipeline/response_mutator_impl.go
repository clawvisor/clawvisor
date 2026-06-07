package pipeline

import (
	"fmt"
	"io"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/conversation/stream"
)

// streamingResponseMutator is the real ResponseMutator implementation
// for the streaming path. It collects mutation intent (one prepend
// text today; substitute later) and applies the result at Commit time
// by piping the upstream body through the per-shape stream codec.
//
// Construct one per response. Call mutator methods to queue mutations
// during the post-phase, then call Commit to stream the transformed
// bytes to the client.
type streamingResponseMutator struct {
	dst   io.Writer
	src   io.ReadCloser
	shape conversation.StreamShape

	prependText    string
	substituteText string
	hasSubstitute  bool
	committed      bool
}

// NewStreamingResponseMutator wires a ResponseMutator that mutates
// the streaming response body via the canonical event-stream model.
// dst is the client connection writer; src is the upstream response
// body reader. The mutator takes ownership of src and closes it when
// committed; shape is the SSE wire shape.
//
// Returns an error for unsupported shapes — only Anthropic Messages
// is fully wired today. OpenAI Chat and Responses follow as their
// shape-specific prepend ports land.
func NewStreamingResponseMutator(dst io.Writer, src io.ReadCloser, shape conversation.StreamShape) (ResponseMutator, error) {
	if dst == nil || src == nil {
		return nil, fmt.Errorf("streaming response mutator: dst and src required")
	}
	switch shape {
	case conversation.StreamShapeAnthropicMessages,
		conversation.StreamShapeOpenAIChat,
		conversation.StreamShapeOpenAIResponses,
		conversation.StreamShapeGoogleGemini:
		// supported
	default:
		return nil, fmt.Errorf("streaming response mutator: unknown shape %v", shape)
	}
	return &streamingResponseMutator{
		dst:   dst,
		src:   src,
		shape: shape,
	}, nil
}

func (m *streamingResponseMutator) PrependAssistantText(text string) error {
	if m.committed {
		return fmt.Errorf("PrependAssistantText after Commit")
	}
	if m.prependText != "" {
		return fmt.Errorf("PrependAssistantText already queued (multiple calls not yet supported)")
	}
	m.prependText = text
	return nil
}

func (m *streamingResponseMutator) SubstituteEntireResponse(text string) error {
	if m.committed {
		return fmt.Errorf("SubstituteEntireResponse after Commit")
	}
	m.hasSubstitute = true
	m.substituteText = text
	return nil
}

// Commit applies the queued mutations and streams the transformed
// response to dst. Safe to call once.
func (m *streamingResponseMutator) Commit() error {
	if m.committed {
		return fmt.Errorf("Commit called twice")
	}
	m.committed = true
	defer func() { _ = m.src.Close() }()

	if m.hasSubstitute {
		// SubstituteEntireResponse takes precedence over Prepend — the
		// upstream response is discarded and replaced with a synthetic
		// one-text-block stream carrying the substitute text.
		switch m.shape {
		case conversation.StreamShapeAnthropicMessages:
			return stream.SubstituteAnthropicResponse(m.dst, m.src, m.substituteText)
		default:
			return fmt.Errorf("SubstituteEntireResponse not yet wired for shape %v", m.shape)
		}
	}

	if m.prependText == "" {
		// No mutations queued — copy upstream verbatim.
		_, err := io.Copy(m.dst, m.src)
		return err
	}

	// Google's prepend isn't wired yet (the stub codec is pass-through
	// only). When a real implementation lands it slots in here.
	if m.shape == conversation.StreamShapeGoogleGemini {
		return fmt.Errorf("streaming response mutator: PrependAssistantText not yet wired for Google Gemini (stub codec is pass-through only)")
	}

	switch m.shape {
	case conversation.StreamShapeAnthropicMessages:
		return stream.PrependAnthropicAssistantNotice(m.dst, m.src, m.prependText)
	case conversation.StreamShapeOpenAIChat:
		return stream.PrependOpenAIChatAssistantNotice(m.dst, m.src, m.prependText)
	case conversation.StreamShapeOpenAIResponses:
		return stream.PrependOpenAIResponsesAssistantNotice(m.dst, m.src, m.prependText)
	default:
		return fmt.Errorf("streaming response mutator: prepend not wired for shape %v", m.shape)
	}
}

// Compile-time assertion: streamingResponseMutator satisfies the
// ResponseMutator interface. The check breaks the build if the
// interface grows without this implementation following.
var _ ResponseMutator = (*streamingResponseMutator)(nil)
