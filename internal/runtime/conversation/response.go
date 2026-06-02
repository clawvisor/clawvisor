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

type ToolUseVerdict struct {
	Allowed        bool
	Reason         string
	SubstituteWith string

	// RewriteInput, when non-nil and Allowed=true, replaces the tool_use's
	// input field in-place. Used by the lite-proxy inspector to redirect
	// the harness's eventual HTTP call at the resolver while preserving
	// the original method/path/body. Per-block mutation; the assistant
	// turn otherwise streams through unchanged.
	RewriteInput json.RawMessage

	// ContinueWithToolResult, when non-empty, signals that the proxy
	// has answered the tool_use itself and wants to feed the result back
	// to the model so it continues with its next tool_use rather than
	// terminating the turn. The handler builds a synthetic user turn
	// containing a tool_result block with this content and re-calls the
	// upstream LLM. SubstituteWith remains the fallback rendered to the
	// harness if the continuation call fails.
	ContinueWithToolResult string

	// PrependAssistantNotice, when non-empty, is text the handler
	// prepends to the assistant turn it returns to the harness AFTER a
	// successful ContinueWithToolResult round-trip. The use case is
	// surfacing a user-facing notice ("a task was auto-approved on
	// your behalf") in the same response that carries the model's next
	// actions, so the user sees what happened without a separate turn.
	// Ignored when ContinueWithToolResult is empty or when the
	// continuation failed and the substitute fallback rendered
	// instead.
	PrependAssistantNotice string

	// CreatedTaskID is set by the conversation auto-approval gate to
	// the ID of the inline task it created before returning the
	// verdict. Carried so downstream audit rows in the lite-proxy
	// handler (e.g. LogContinuationSkippedSiblingTools when sibling
	// tool_uses force a fallback) can link to the same task_id the
	// rest of the approval audit trail uses — without parsing the
	// augmentation text or threading a separate map. Empty for any
	// other verdict source.
	CreatedTaskID string
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

// ThinkingRewriteRefusal converts a verdict that wanted to rewrite a
// tool_use input into a blocked verdict, used by Anthropic response
// rewriters (both buffered and streaming) when the assistant message
// contains a thinking or redacted_thinking block.
//
// Anthropic's signature on a thinking block covers every sibling
// block in the assistant message, including each tool_use.input.
// Modifying an input in place invalidates the signature; the agent
// CLI then echoes the modified content back on the next turn and
// Anthropic returns:
//
//	400 invalid_request_error: `thinking` or `redacted_thinking` blocks
//	in the latest assistant message cannot be modified.
//
// The refusal carries the SAME message in Reason and
// ContinueWithToolResult so the recovery surfaces both as a
// refusal-text and as a synthetic tool_result on the continuation
// path — same shape the rewriter failure path already uses for the
// no-rewriter-for-shape case.
//
// Exported so the lite-proxy's streaming postprocess loop (which
// applies rewrites AFTER StreamRewrite returns) can call it with the
// same shape as the buffered rewriters.
func ThinkingRewriteRefusal(orig ToolUseVerdict) ToolUseVerdict {
	const reason = "Clawvisor: credentialed rewrite declined — extended thinking is active on this assistant turn. " +
		"Rewriting the tool_use input in place would invalidate the model's thinking-block signature, " +
		"and Anthropic would 400 the next request with `thinking blocks in the latest assistant message cannot be modified`. " +
		"To proceed: retry without extended thinking, or restructure so the credentialed call doesn't need an in-place rewrite (e.g. have the agent construct the proxy URL itself rather than calling the upstream host directly)."
	return ToolUseVerdict{
		Allowed:                false,
		Reason:                 reason,
		ContinueWithToolResult: reason,
		// Carry through any CreatedTaskID so audit linkage stays
		// intact even though the rewrite was declined.
		CreatedTaskID: orig.CreatedTaskID,
	}
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

type ContinuationToolResult struct {
	ToolUseID string
	Content   string
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

	// HasThinking is true when the streamed assistant turn included a
	// thinking or redacted_thinking content block. The post-stream
	// rewrite loop in lite-proxy MUST NOT apply any tool_use input
	// rewrite when this is set — the model's signature covers every
	// sibling block, including tool_use inputs, so an in-place rewrite
	// invalidates it and the agent's NEXT request 400s with
	// "thinking blocks in the latest assistant message cannot be
	// modified." Callers should convert any RewriteInput-bearing
	// verdict into a ThinkingRewriteRefusal-style block result when
	// this is true.
	HasThinking bool
}

type ResponseRewriter interface {
	Name() Provider
	MatchesResponse(req *http.Request, resp *http.Response) bool
	Rewrite(body []byte, contentType string, eval ToolUseEvaluator) (RewriteResult, error)
}

type StreamingResponseRewriter interface {
	Name() Provider
	MatchesResponse(req *http.Request, resp *http.Response) bool
	StreamRewrite(ctx context.Context, r io.Reader, w io.Writer) (StreamingRewriteResult, error)
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
