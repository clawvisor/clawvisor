package conversation

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

type ToolUseEvaluator func(ToolUse) ToolUseVerdict

// ToolUseVerdict is the unified per-tool-use verdict shape consumed by
// response rewriters AND produced by the policy pipeline. Both pipelines
// share this type — there is no separate pipeline.ToolUseVerdict.
type ToolUseVerdict struct {
	// Allowed is the compatibility boolean derived from Outcome.
	// Rewriters read this directly. Set true when Outcome is
	// OutcomeAllow or OutcomeRewrite; false otherwise.
	Allowed bool
	// Outcome is the typed verdict category produced by pipeline
	// evaluators. Optional for callers that only set Allowed.
	Outcome Outcome
	Reason  string

	// SubstituteWith replaces the tool_use block with a plain-text
	// assistant block in the rewritten response. Used by approval-prompt
	// rendering, inline-task interception, etc.
	SubstituteWith string
	// SubstituteWithToolCall, when non-nil, replaces the blocked
	// tool_use block with a synthetic tool_use of the named tool and
	// supplied input — instead of a text block. The inline-approval
	// path uses this to swap the model's POST /api/control/tasks call
	// for an AskUserQuestion tool_use so the harness can surface the
	// yes/no through its structured picker UI rather than as a chat
	// message the user has to read and reply to in text.
	//
	// When BOTH SubstituteWith and SubstituteWithToolCall are set the
	// rewriters MUST prefer SubstituteWithToolCall — SubstituteWith
	// stays populated as a back-compat fallback for adapters that
	// haven't been taught the new shape yet (audit serialization,
	// non-Anthropic providers).
	SubstituteWithToolCall *SyntheticToolCall
	// SuppressSubstituteText, when true and Allowed=false, prevents the
	// rewriter/formatter from falling back to a default "Tool 'X' was
	// blocked by Clawvisor policy: ..." text when SubstituteWith is empty.
	// Used during coalesced approval turns where sibling tools should
	// not render their own separate block messages.
	SuppressSubstituteText bool

	// RewriteInput, when non-nil, replaces the tool_use's input field
	// in-place. Used by the lite-proxy inspector to redirect the
	// harness's eventual HTTP call at the resolver while preserving
	// the original method/path/body.
	RewriteInput json.RawMessage

	// RecoverableReason marks the verdict as a recoverable-deny that
	// the postproc eval wrapper should transform into the canonical
	// placeholder + pending-substitution pattern. The reason text is
	// what lands as the tool_result content on the next inbound
	// /v1/messages; the original tool_use is restored byte-for-byte so
	// the model sees its own call answered with the failure reason and
	// can retry with a corrected shape. Used by RecoverableDenyVerdict
	// callers — inspector parse errors, boundary check failures,
	// credential rewrite errors, etc.
	RecoverableReason string

	// CreatedTaskID names the inline task created by the
	// conversation auto-approval gate. Carried so downstream audit
	// rows can link to the same task_id.
	CreatedTaskID string

	// HeldKindHint is the policy-set classification of this verdict
	// for postproc's coalescing pass. When empty, classification falls
	// back to the Allowed / RewriteInput shape.
	HeldKindHint HeldKindHint

	// --- response orchestration fields ---

	// HoldKey groups sibling tool_uses for coalescing. Empty means
	// "do not coalesce" (each Hold gets its own approval row).
	HoldKey string

	// Facts carries typed observations the evaluator emitted. Audit
	// emission branches via type switch on Facts. Populated for EVERY
	// evaluator that runs, including those returning Skip —
	// observation is a separate channel from verdict claiming.
	Facts []EvaluationFact
}

type RewriteResult struct {
	Body          []byte
	Decisions     []ToolUseDecisionRecord
	Rewritten     bool
	AssistantTurn *Turn
}

type ToolUseDecisionRecord struct {
	ToolUse          ToolUse
	Verdict          ToolUseVerdict
	ToolInputPreview string
}

const toolInputPreviewLimit = 512

func MakeToolInputPreview(in json.RawMessage) string {
	if len(in) == 0 {
		return ""
	}
	s := string(in)
	if len(s) <= toolInputPreviewLimit {
		return s
	}
	return s[:toolInputPreviewLimit] + "..."
}

type StreamingRewriteResult struct {
	ToolUses                  []ToolUse
	AssistantTurn             *Turn
	StreamID                  string
	Model                     string
	Role                      string
	StreamFormat              string
	NextAnthropicContentIndex int
	NextOpenAIOutputIndex     int
}

type ResponseRewriter interface {
	Name() Provider
	MatchesResponse(req *http.Request, resp *http.Response) bool
	Rewrite(body []byte, contentType string, eval ToolUseEvaluator) (RewriteResult, error)
}

type StreamingResponseRewriter interface {
	Name() Provider
	MatchesResponse(req *http.Request, resp *http.Response) bool
	// StreamRewrite reads the upstream SSE stream from r, writes the
	// rewritten (or unchanged) stream to w, and returns the per-stream
	// summary the post-pass needs (tool_uses observed, indices, IDs).
	//
	// onToolUse, if non-nil, is invoked as each tool_use's parsing
	// completes (content_block_stop for Anthropic; the equivalent for
	// OpenAI). Streaming callers use this to collect tool_uses as they
	// arrive; the returned result still carries the full ToolUses slice
	// for callers that don't supply a callback.
	StreamRewrite(ctx context.Context, r io.Reader, w io.Writer, onToolUse func(ToolUse)) (StreamingRewriteResult, error)
}

type ResponseRegistry struct {
	rewriters []ResponseRewriter
}

func NewResponseRegistry(rewriters ...ResponseRewriter) *ResponseRegistry {
	return &ResponseRegistry{rewriters: rewriters}
}

func DefaultResponseRegistry() *ResponseRegistry {
	return NewResponseRegistry(
		&AnthropicResponseRewriter{},
		&OpenAIResponseRewriter{},
	)
}

func (r *ResponseRegistry) ForProviderStreaming(p Provider) StreamingResponseRewriter {
	rw := r.ForProvider(p)
	if rw == nil {
		return nil
	}
	if srw, ok := rw.(StreamingResponseRewriter); ok {
		return srw
	}
	return nil
}

func (r *ResponseRegistry) Match(req *http.Request, resp *http.Response) ResponseRewriter {
	if r == nil {
		return nil
	}
	for _, rewriter := range r.rewriters {
		if rewriter.MatchesResponse(req, resp) {
			return rewriter
		}
	}
	return nil
}

// ForProvider returns the registered rewriter for a given provider. The
// runtime proxy uses Match(req, resp) which keys off the upstream host;
// the lite-proxy dispatches by route instead and needs an explicit lookup.
func (r *ResponseRegistry) ForProvider(p Provider) ResponseRewriter {
	if r == nil {
		return nil
	}
	for _, rewriter := range r.rewriters {
		if rewriter.Name() == p {
			return rewriter
		}
	}
	return nil
}

type assistantFragment struct {
	IsTool   bool
	Text     string
	ToolName string
	ToolArgs json.RawMessage
}

func formatAssistantContent(frags []assistantFragment) string {
	var b strings.Builder
	for i, frag := range frags {
		if i > 0 {
			b.WriteByte('\n')
		}
		if frag.IsTool {
			b.WriteString("<tool_use name=")
			b.WriteString(frag.ToolName)
			if len(frag.ToolArgs) > 0 {
				b.WriteString(" input=")
				b.Write(frag.ToolArgs)
			}
			b.WriteByte('>')
			continue
		}
		b.WriteString(frag.Text)
	}
	return b.String()
}

func assistantTurnFromFragments(frags []assistantFragment, decisions []ToolUseDecisionRecord) *Turn {
	final := applyBlockSubstitutions(frags, decisions)
	content := formatAssistantContent(final)
	if content == "" {
		return nil
	}
	return &Turn{Role: RoleAssistant, Content: content}
}

func applyBlockSubstitutions(frags []assistantFragment, decisions []ToolUseDecisionRecord) []assistantFragment {
	if len(decisions) == 0 {
		return frags
	}
	out := make([]assistantFragment, 0, len(frags))
	toolDecisionIdx := 0
	for _, frag := range frags {
		if !frag.IsTool {
			out = append(out, frag)
			continue
		}
		if toolDecisionIdx >= len(decisions) {
			out = append(out, frag)
			continue
		}
		decision := decisions[toolDecisionIdx]
		toolDecisionIdx++
		if !decision.Verdict.Allowed {
			if substitute := strings.TrimSpace(decision.Verdict.SubstituteWith); substitute != "" {
				out = append(out, assistantFragment{Text: substitute})
				continue
			}
			if decision.Verdict.SuppressSubstituteText {
				continue
			}
			reason := decision.Verdict.Reason
			if reason == "" {
				reason = "blocked by policy"
			}
			out = append(out, assistantFragment{
				Text: fmt.Sprintf("Tool '%s' was blocked by Clawvisor policy: %s", frag.ToolName, reason),
			})
			continue
		}
		out = append(out, frag)
	}
	return out
}

func BlockedReasonText(decisions []ToolUseDecisionRecord) string {
	var substitutions []string
	for _, decision := range decisions {
		if decision.Verdict.SuppressSubstituteText {
			continue
		}
		if decision.Verdict.SubstituteWith != "" {
			substitutions = append(substitutions, decision.Verdict.SubstituteWith)
		}
	}
	if len(substitutions) > 0 {
		return strings.Join(substitutions, "\n\n")
	}

	var parts []string
	for _, decision := range decisions {
		if decision.Verdict.Allowed {
			continue
		}
		if decision.Verdict.SuppressSubstituteText {
			continue
		}
		reason := decision.Verdict.Reason
		if reason == "" {
			reason = "blocked by policy"
		}
		parts = append(parts, fmt.Sprintf("- %s: %s", decision.ToolUse.Name, reason))
	}
	if len(parts) == 0 {
		return ""
	}
	return "Tool use was blocked by the Clawvisor proxy:\n" + strings.Join(parts, "\n")
}

func blockedReasonTextForAssistant(decisions []ToolUseDecisionRecord) string {
	text := strings.TrimSpace(BlockedReasonText(decisions))
	if text != "" {
		return text
	}
	return "Tool use was blocked by the Clawvisor proxy."
}

func isSSE(contentType string) bool {
	ct := strings.ToLower(strings.TrimSpace(contentType))
	return strings.HasPrefix(ct, "text/event-stream")
}

// IsSSEContentType reports whether the given Content-Type is an SSE
// stream. Exported so sibling packages (the lite-proxy handler in
// particular) can branch on wire format without duplicating the prefix
// check.
func IsSSEContentType(contentType string) bool { return isSSE(contentType) }

func matchAnthropicEndpoint(req *http.Request) bool {
	if req == nil || req.URL == nil {
		return false
	}
	host := strings.ToLower(hostFromRequest(req))
	return host == "api.anthropic.com" && strings.HasPrefix(req.URL.Path, "/v1/messages")
}

func MatchProviderAnthropic(req *http.Request) bool {
	return matchAnthropicEndpoint(req)
}
