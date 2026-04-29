package conversation

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

type ToolUseEvaluator func(ToolUse) ToolUseVerdict

type ToolUseVerdict struct {
	Allowed        bool
	Reason         string
	SubstituteWith string
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

type ResponseRewriter interface {
	Name() Provider
	MatchesResponse(req *http.Request, resp *http.Response) bool
	Rewrite(body []byte, contentType string, eval ToolUseEvaluator) (RewriteResult, error)
}

type ResponseRegistry struct {
	rewriters []ResponseRewriter
}

func DefaultResponseRegistry() *ResponseRegistry {
	return &ResponseRegistry{rewriters: []ResponseRewriter{
		&AnthropicResponseRewriter{},
	}}
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
	denied := map[string]string{}
	for _, decision := range decisions {
		if decision.Verdict.Allowed {
			continue
		}
		reason := decision.Verdict.Reason
		if reason == "" {
			reason = "blocked by policy"
		}
		denied[decision.ToolUse.Name] = reason
	}
	if len(denied) == 0 {
		return frags
	}
	out := make([]assistantFragment, 0, len(frags))
	for _, frag := range frags {
		if !frag.IsTool {
			out = append(out, frag)
			continue
		}
		if reason, ok := denied[frag.ToolName]; ok {
			out = append(out, assistantFragment{
				Text: fmt.Sprintf("Tool '%s' was blocked by Clawvisor policy: %s", frag.ToolName, reason),
			})
			continue
		}
		out = append(out, frag)
	}
	return out
}

func blockedReasonText(decisions []ToolUseDecisionRecord) string {
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

func isSSE(contentType string) bool {
	ct := strings.ToLower(strings.TrimSpace(contentType))
	return strings.HasPrefix(ct, "text/event-stream")
}

func matchAnthropicEndpoint(req *http.Request) bool {
	if req == nil || req.URL == nil {
		return false
	}
	host := strings.ToLower(hostFromRequest(req))
	return host == "api.anthropic.com" && strings.HasPrefix(req.URL.Path, "/v1/messages")
}
