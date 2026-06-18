package conversation

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
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
	// the postproc eval wrapper transforms into the canonical
	// placeholder + pending-substitution pattern. The reason text is
	// what lands as the tool_result content on the next inbound
	// /v1/messages; the original tool_use is restored byte-for-byte so
	// the model sees its own call answered with the failure reason and
	// can retry with a corrected shape. Used by RecoverableDenyVerdict
	// callers — inspector parse errors, boundary check failures,
	// credential rewrite errors, etc.
	//
	// Contract: this field is a PRODUCER → POSTPROC signal. Policy
	// evaluators populate it; postproc.transformRecoverableDenyToPlaceholder
	// (in the eval wrapper) consumes it and rewrites the verdict in
	// place — clearing RecoverableReason and setting
	// SubstituteWithToolCall + SuppressSubstituteText before any
	// renderer sees the verdict. Response rewriters MUST NOT branch on
	// this field; they only see the post-transform shape.
	// SubstituteWith stays populated as a terminal fallback for the
	// case where the registry isn't wired (e.g., test fixtures without
	// AuthorizationContext.ScopeDrifts).
	RecoverableReason string

	// CreatedTaskID names the inline task created by the
	// conversation auto-approval gate. Carried so downstream audit
	// rows can link to the same task_id.
	CreatedTaskID string

	// PendingSubstitution, when non-nil, declares that the postprocess
	// layer should register an inbound substitution after the verdict
	// is finalized. Evaluators MUST NOT write to the substitution
	// registry directly — populate this field and let the postprocess
	// layer own the registry write and its rollback. This restores
	// the "verdict is pure data" invariant: the verdict describes
	// what should happen; postprocess realizes it.
	PendingSubstitution *PendingSubstitutionSpec

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

// PendingSubstitutionSpec describes an inbound substitution the
// postprocess layer should register after the verdict is finalized.
// The spec is pure data: evaluators populate it; postprocess realizes
// it. Provider-specific marshaling and registry keying happen in
// postprocess, so this struct stays free of llmproxy-package
// dependencies.
//
// Fields:
//   - DriftID: scope-drift record this substitution is associated
//     with (mint path), empty for paths that don't mint a drift
//     (recoverable-deny).
//   - MenuText: content the inbound rewriter splices into the
//     tool_result on the next /v1/messages.
//   - OriginalToolName / OriginalToolInput: the model's original
//     tool_use fields, restored by the inbound rewriter into the
//     prior assistant turn so the model never sees the harness-side
//     placeholder.
//   - TaskRollback: when non-nil, the inline task to expire if the
//     registry write fails. Used by auto-approve so a failed
//     registration unwinds the orphan task created earlier in the
//     evaluator.
type PendingSubstitutionSpec struct {
	DriftID           string
	MenuText          string
	OriginalToolName  string
	OriginalToolInput []byte
	TaskRollback      *PendingSubstitutionTaskRollback
}

// PendingSubstitutionTaskRollback names an inline task the postprocess
// layer must expire if the substitution registry write fails. Carried
// by the auto-approve path. The expirer itself lives in the
// PostprocessConfig (InlineTaskCreator); the spec only carries the
// identity tuple needed to invoke it.
type PendingSubstitutionTaskRollback struct {
	TaskID string
	UserID string
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

// InboundSubstitutionLookup is the read-only registry view the
// InboundRewriter consults to find pending substitutions keyed by
// (agent, conversation, tool_use_id). The lookup is non-consuming —
// the harness's stored history carries the placeholder for the rest
// of the conversation, so every subsequent inbound request has to
// restore it. The lifetime is owned by the postprocess layer that
// registered the entry; the rewriter is purely a reader.
type InboundSubstitutionLookup interface {
	LookupPendingSubstitution(ctx context.Context, agentID, conversationID, toolUseID string) (InboundPendingSubstitution, bool)
}

// InboundPendingSubstitution is the rewriter's view of a registry
// entry. Mirrors the fields PendingSubstitutionSpec carried into the
// registry on the response leg, lifted into the conversation package
// so the rewriter doesn't depend on llmproxy.
type InboundPendingSubstitution struct {
	DriftID           string
	MenuText          string
	OriginalToolName  string
	OriginalToolInput []byte
}

// InboundRewriteRequest is the input to InboundRewriter.RewriteInbound.
// Identifying context (Agent + ConversationID) keys into the
// substitution lookup; Body + Provider drive the JSON shape walk.
type InboundRewriteRequest struct {
	HTTPRequest    *http.Request
	Provider       Provider
	Body           []byte
	AgentID        string
	AgentUserID    string
	ConversationID string
	Lookup         InboundSubstitutionLookup
	Logger         *slog.Logger
}

// InboundRewriteResult reports what RewriteInbound did. Rewritten=false
// + nil error means no pending substitutions applied to the body.
type InboundRewriteResult struct {
	Body            []byte
	Rewritten       bool
	AppliedDriftIDs []string
}

// InboundRewriter is the per-provider abstraction for the inbound
// /v1/messages (or /v1/responses, /v1/chat/completions) walker that
// restores model-original tool_use blocks and splices menu text into
// the matching tool_result on retry. Mirrors ResponseRewriter on the
// outbound leg so dispatch, testing, and registry shape stay
// symmetric.
type InboundRewriter interface {
	Name() Provider
	// MatchesInbound returns true when this rewriter knows how to
	// walk the request's body shape. Provider routing is the
	// primary signal — Anthropic vs OpenAI — with sub-shape
	// disambiguation (Chat Completions vs Responses) handled inside
	// RewriteInbound where the body bytes are already in scope.
	MatchesInbound(req *http.Request) bool
	RewriteInbound(ctx context.Context, req InboundRewriteRequest) (InboundRewriteResult, error)
}

// InboundRegistry dispatches inbound bodies to the matching
// InboundRewriter. Parallel to ResponseRegistry so callers can route
// either leg through one canonical Match() lookup.
type InboundRegistry struct {
	rewriters []InboundRewriter
}

func NewInboundRegistry(rewriters ...InboundRewriter) *InboundRegistry {
	return &InboundRegistry{rewriters: rewriters}
}

// Match returns the first registered rewriter that claims the
// request, or nil. Provider routing happens via MatchesInbound; the
// caller's only job is to feed the *http.Request.
func (r *InboundRegistry) Match(req *http.Request) InboundRewriter {
	if r == nil {
		return nil
	}
	for _, rewriter := range r.rewriters {
		if rewriter.MatchesInbound(req) {
			return rewriter
		}
	}
	return nil
}

// ForProvider returns the registered rewriter for the given provider,
// or nil. Parallels ResponseRegistry.ForProvider for callers that
// dispatch by provider name (lite-proxy route resolver, tests).
func (r *InboundRegistry) ForProvider(p Provider) InboundRewriter {
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
			if decision.Verdict.SubstituteWithToolCall != nil &&
				frag.ToolName == decision.Verdict.SubstituteWithToolCall.Name {
				// Placeholder substitution (scope-drift, recoverable-
				// deny, auto-approve) rendered as a synthetic tool_use
				// on the wire: the rewriter populated frag.ToolName
				// with the substitute's Name, so the match confirms the
				// wire actually carries the tool_use shape. Pass the
				// fragment through unchanged so the AssistantTurn audit
				// matches. Any preamble SubstituteWith text was
				// emitted as its own preceding Text frag by the
				// rewriter and passes through above.
				out = append(out, frag)
				continue
			}
			// Reaches here when either (a) no SubstituteWithToolCall
			// was set, or (b) one was set but the wire-side renderer
			// (anthropicSubstituteToolUseBlock / emitOpenAIResponsesSubstitute)
			// returned ok=false and the rewriter fell back to a text
			// content block — leaving the trailing frag with the
			// ORIGINAL tool name. The wire-side fallback mirrors the
			// text-derivation rules below (SubstituteWith → policy
			// default → suppression unless paired with a
			// SubstituteWithToolCall escape hatch), so re-apply the
			// same precedence here to keep audit shape aligned with
			// the wire.
			if substitute := strings.TrimSpace(decision.Verdict.SubstituteWith); substitute != "" {
				out = append(out, assistantFragment{Text: substitute})
				continue
			}
			// SuppressSubstituteText only silences when there's no
			// SubstituteWithToolCall fallback in play. When a fallback
			// IS in play, the wire emits a default policy notice
			// (escape hatch in the per-provider rewriters); the audit
			// follows suit.
			if decision.Verdict.SuppressSubstituteText && decision.Verdict.SubstituteWithToolCall == nil {
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
