package pipeline_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/pipeline"
)

// orchTestResponse is a minimal ReadOnlyResponse for orchestrator tests.
type orchTestResponse struct {
	provider conversation.Provider
	shape    conversation.StreamShape
}

func (r *orchTestResponse) Provider() conversation.Provider       { return r.provider }
func (r *orchTestResponse) StreamShape() conversation.StreamShape { return r.shape }
func (r *orchTestResponse) IsStreaming() bool                     { return true }
func (r *orchTestResponse) ToolUses() []conversation.ToolUse      { return nil }

// prependPolicy queues a PrependAssistantText with the given text.
type prependPolicy struct {
	name string
	text string
}

func (p *prependPolicy) Name() string { return p.name }
func (p *prependPolicy) Postprocess(_ context.Context, _ pipeline.ReadOnlyResponse, mut pipeline.ResponseMutator) (pipeline.ResponseVerdict, error) {
	if p.text == "" {
		return pipeline.ResponseVerdict{Outcome: pipeline.OutcomeSkip}, nil
	}
	if err := mut.PrependAssistantText(p.text); err != nil {
		return pipeline.ResponseVerdict{}, err
	}
	return pipeline.ResponseVerdict{
		Outcome:     pipeline.OutcomeAllow,
		AuditParams: map[string]any{p.name + "_ran": true},
	}, nil
}

// erroringResponsePolicy errors during Postprocess.
type erroringResponsePolicy struct{ name string }

func (p *erroringResponsePolicy) Name() string { return p.name }
func (p *erroringResponsePolicy) Postprocess(_ context.Context, _ pipeline.ReadOnlyResponse, _ pipeline.ResponseMutator) (pipeline.ResponseVerdict, error) {
	return pipeline.ResponseVerdict{}, errors.New("explode")
}

// denyingResponsePolicy returns an unsupported outcome to verify the
// orchestrator's "Deny isn't supported in postprocess" check.
type denyingResponsePolicy struct{ name string }

func (p *denyingResponsePolicy) Name() string { return p.name }
func (p *denyingResponsePolicy) Postprocess(_ context.Context, _ pipeline.ReadOnlyResponse, _ pipeline.ResponseMutator) (pipeline.ResponseVerdict, error) {
	return pipeline.ResponseVerdict{Outcome: pipeline.OutcomeDeny}, nil
}

// TestRunPost_PrependsThroughStream verifies the happy path: one
// ResponsePolicy queues PrependAssistantText, commit streams the
// transformed response to dst.
func TestRunPost_PrependsThroughStream(t *testing.T) {
	upstream := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_x","role":"assistant","model":"claude-sonnet-4"}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")
	const notice = "[Clawvisor] notice"

	var dst bytes.Buffer
	res := &orchTestResponse{
		provider: conversation.ProviderAnthropic,
		shape:    conversation.StreamShapeAnthropicMessages,
	}
	policies := []pipeline.ResponsePolicy{
		&prependPolicy{name: "agent_notice", text: notice},
	}

	result, err := pipeline.RunPost(
		context.Background(),
		res,
		&dst,
		io.NopCloser(strings.NewReader(upstream)),
		conversation.StreamShapeAnthropicMessages,
		policies,
	)
	if err != nil {
		t.Fatalf("RunPost: %v", err)
	}

	got := dst.String()
	if !strings.Contains(got, notice) {
		t.Errorf("notice missing from streamed output:\n%s", got)
	}
	if !strings.Contains(got, "hello") {
		t.Errorf("upstream hello text lost:\n%s", got)
	}
	if result.AuditParams["agent_notice_ran"] != true {
		t.Errorf("audit field agent_notice_ran missing: %+v", result.AuditParams)
	}
	if len(result.Verdicts) != 1 {
		t.Errorf("expected 1 verdict, got %d", len(result.Verdicts))
	}
}

// TestRunPost_SkipPolicyDoesNotMutate verifies that a policy emitting
// Skip leaves the stream unchanged.
func TestRunPost_SkipPolicyDoesNotMutate(t *testing.T) {
	upstream := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start"}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")

	var dst bytes.Buffer
	res := &orchTestResponse{
		provider: conversation.ProviderAnthropic,
		shape:    conversation.StreamShapeAnthropicMessages,
	}
	policies := []pipeline.ResponsePolicy{
		&prependPolicy{name: "skipper", text: ""}, // empty text → Skip
	}

	if _, err := pipeline.RunPost(
		context.Background(),
		res,
		&dst,
		io.NopCloser(strings.NewReader(upstream)),
		conversation.StreamShapeAnthropicMessages,
		policies,
	); err != nil {
		t.Fatalf("RunPost: %v", err)
	}

	// Skip means no mutation queued; the streamingResponseMutator's
	// Commit should copy upstream verbatim.
	if got := dst.String(); got != upstream {
		t.Errorf("Skip policy mutated stream\n--- want ---\n%s\n--- got ---\n%s", upstream, got)
	}
}

// TestRunPost_PropagatesPolicyError verifies error propagation.
func TestRunPost_PropagatesPolicyError(t *testing.T) {
	var dst bytes.Buffer
	res := &orchTestResponse{provider: conversation.ProviderAnthropic, shape: conversation.StreamShapeAnthropicMessages}
	policies := []pipeline.ResponsePolicy{
		&erroringResponsePolicy{name: "exploder"},
	}
	_, err := pipeline.RunPost(
		context.Background(),
		res,
		&dst,
		io.NopCloser(strings.NewReader("event: x\ndata: {}\n\n")),
		conversation.StreamShapeAnthropicMessages,
		policies,
	)
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
}

// TestRunPost_RejectsDeny pins the design choice: Deny in postprocess
// doesn't have a sensible semantic (the upstream already responded).
// The orchestrator refuses it loudly.
func TestRunPost_RejectsDeny(t *testing.T) {
	var dst bytes.Buffer
	res := &orchTestResponse{provider: conversation.ProviderAnthropic, shape: conversation.StreamShapeAnthropicMessages}
	policies := []pipeline.ResponsePolicy{
		&denyingResponsePolicy{name: "deny"},
	}
	_, err := pipeline.RunPost(
		context.Background(),
		res,
		&dst,
		io.NopCloser(strings.NewReader("event: x\ndata: {}\n\n")),
		conversation.StreamShapeAnthropicMessages,
		policies,
	)
	if err == nil {
		t.Fatalf("expected error for Deny outcome, got nil")
	}
}
