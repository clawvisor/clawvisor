package slack

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/clawvisor/clawvisor/internal/display"
	"github.com/clawvisor/clawvisor/pkg/notify"
)

// Slack hard limits. Exceeding either makes chat.postMessage reject the whole
// message, so every builder truncates rather than risking a silent drop of an
// approval prompt.
const (
	maxSectionText = 2900 // Slack's cap is 3000; leave room for the ellipsis.
	maxBlocks      = 48   // Slack's cap is 50; leave room for the actions block.
)

// block is one Block Kit block. Slack's schema is heavily polymorphic, so
// this stays a map rather than a union of typed structs.
type block map[string]any

// esc escapes the three characters Slack treats as markup control characters.
// Unlike Telegram's HTML mode, everything else (including quotes) is literal.
func esc(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	return r.Replace(s)
}

// truncate clips s to n runes, appending an ellipsis when it had to cut.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// section builds a mrkdwn section.
//
// It does NOT escape: callers own escaping, because most build their text by
// interpolating already-escaped fragments (see esc) and escaping again here
// would double-encode them. Text arriving from telegramHTMLToMrkdwn is
// likewise already escaped.
func section(md string) block {
	return block{
		"type": "section",
		"text": map[string]any{"type": "mrkdwn", "text": truncate(md, maxSectionText)},
	}
}

func contextBlock(md string) block {
	return block{
		"type": "context",
		"elements": []any{
			map[string]any{"type": "mrkdwn", "text": truncate(md, maxSectionText)},
		},
	}
}

func header(text string) block {
	// plain_text headers are capped at 150 chars and render no markup.
	return block{
		"type": "header",
		"text": map[string]any{"type": "plain_text", "text": truncate(text, 148), "emoji": true},
	}
}

func divider() block { return block{"type": "divider"} }

// fields renders label/value pairs into Slack's two-column field layout.
// Slack allows at most 10 fields per section.
func fields(pairs ...[2]string) block {
	out := make([]any, 0, len(pairs))
	for i, p := range pairs {
		if i == 10 {
			break
		}
		out = append(out, map[string]any{
			"type": "mrkdwn",
			"text": truncate(fmt.Sprintf("*%s*\n%s", esc(p[0]), esc(p[1])), 1900),
		})
	}
	return block{"type": "section", "fields": out}
}

// approveDenyActions builds the button row. approveToken and denyToken are
// single-use callback tokens; they are the authorization for the decision,
// so they travel as the button value and never as an action_id.
func approveDenyActions(approveToken, denyToken, denyLabel string) block {
	return block{
		"type": "actions",
		"elements": []any{
			map[string]any{
				"type":      "button",
				"action_id": actionApprove,
				"style":     "primary",
				"text":      map[string]any{"type": "plain_text", "text": "Approve", "emoji": true},
				"value":     approveToken,
			},
			map[string]any{
				"type":      "button",
				"action_id": actionDeny,
				"style":     "danger",
				"text":      map[string]any{"type": "plain_text", "text": denyLabel, "emoji": true},
				"value":     denyToken,
			},
		},
	}
}

// linkActions builds a row of URL buttons, used when token minting failed and
// we fall back to deep links into the dashboard.
func linkActions(approveURL, denyURL string) block {
	elems := []any{}
	if approveURL != "" {
		elems = append(elems, map[string]any{
			"type": "button", "style": "primary",
			"text": map[string]any{"type": "plain_text", "text": "Approve", "emoji": true},
			"url":  approveURL, "action_id": actionNoopApprove,
		})
	}
	if denyURL != "" {
		elems = append(elems, map[string]any{
			"type": "button", "style": "danger",
			"text": map[string]any{"type": "plain_text", "text": "Deny", "emoji": true},
			"url":  denyURL, "action_id": actionNoopDeny,
		})
	}
	return block{"type": "actions", "elements": elems}
}

// clamp drops blocks beyond Slack's per-message limit, leaving a marker so a
// truncated prompt is never mistaken for the whole request.
func clamp(blocks []block) []block {
	if len(blocks) <= maxBlocks {
		return blocks
	}
	out := blocks[:maxBlocks-1]
	return append(out, contextBlock(":warning: _Some details were omitted — open the dashboard for the full request._"))
}

func stamp() string {
	return time.Now().UTC().Format("Mon Jan 2 2006, 3:04 PM MST")
}

// ── Approval ──────────────────────────────────────────────────────────────────

func approvalBlocks(req notify.ApprovalRequest) []block {
	b := []block{
		header("🔔 Approval Request"),
		fields(
			[2]string{"Agent", req.AgentName},
			[2]string{"Service", display.ServiceName(req.Service)},
			[2]string{"Action", display.ActionName(req.Action)},
			[2]string{"Time", stamp()},
		),
	}

	if len(req.Params) > 0 {
		keys := make([]string, 0, len(req.Params))
		for k := range req.Params {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var sb strings.Builder
		sb.WriteString("*Parameters*\n```")
		for _, k := range keys {
			fmt.Fprintf(&sb, "\n%s: %s", k, paramValue(req.Params[k]))
		}
		sb.WriteString("\n```")
		b = append(b, section(sb.String()))
	}

	if req.Reason != "" {
		b = append(b, section(fmt.Sprintf("*Agent's stated reason*\n>%s", esc(req.Reason))))
	}

	if hasVerificationWarning(req) {
		b = append(b, section("*🔍 Verification warning*\n"+verificationLines(req)))
	}

	if req.PolicyReason != "" {
		b = append(b, contextBlock(":warning: "+esc(req.PolicyReason)))
	} else {
		b = append(b, contextBlock(":warning: No policy covers this action."))
	}
	if req.ExpiresIn != "" {
		b = append(b, contextBlock(fmt.Sprintf("Expires in %s", esc(req.ExpiresIn))))
	}
	return b
}

// verificationLines renders the verifier's verdicts. Every non-empty verdict
// gets a line, including ones this renderer does not recognise ("n/a" and
// anything a future verifier adds): a warning section that names no finding
// tells the reviewer nothing, and silently dropping a verdict would hide the
// very fact that raised the warning.
func verificationLines(req notify.ApprovalRequest) string {
	var sb strings.Builder
	switch req.VerifyParamScope {
	case "":
	case "violation":
		sb.WriteString("• :x: *param_scope:* violation\n")
	case "ok":
		sb.WriteString("• :white_check_mark: param_scope: ok\n")
	default:
		sb.WriteString("• :warning: *param_scope:* " + esc(req.VerifyParamScope) + "\n")
	}
	switch req.VerifyReasonCoherence {
	case "":
	case "incoherent":
		sb.WriteString("• :x: *reason:* incoherent\n")
	case "insufficient":
		sb.WriteString("• :warning: *reason:* insufficient\n")
	case "ok":
		sb.WriteString("• :white_check_mark: reason: ok\n")
	default:
		sb.WriteString("• :warning: *reason:* " + esc(req.VerifyReasonCoherence) + "\n")
	}
	if req.VerifyExplanation != "" {
		sb.WriteString("• :speech_balloon: " + esc(req.VerifyExplanation))
	}
	return strings.TrimRight(sb.String(), "\n")
}

// hasVerificationWarning mirrors the Telegram renderer's predicate exactly:
// anything other than a clean "ok" on both axes is surfaced. Enumerating the
// known-bad verdicts instead would drop new or inconclusive ones such as
// "n/a" — and on an approval prompt, under-reporting a verification result is
// the dangerous direction to be wrong in.
func hasVerificationWarning(req notify.ApprovalRequest) bool {
	if req.VerifyParamScope == "" && req.VerifyReasonCoherence == "" {
		return false // verification did not run
	}
	return req.VerifyParamScope != "ok" || req.VerifyReasonCoherence != "ok"
}

// ── Task approval ─────────────────────────────────────────────────────────────

func taskApprovalBlocks(req notify.TaskApprovalRequest) []block {
	f := [][2]string{
		{"Agent", req.AgentName},
		{"Time", stamp()},
	}
	if req.RiskLevel != "" && req.RiskLevel != "unknown" {
		f = append(f, [2]string{"Risk", riskEmoji(req.RiskLevel) + " " + req.RiskLevel})
	}

	b := []block{
		header("📋 Task Approval Request"),
		section("*Purpose*\n" + esc(req.Purpose)),
		fields(f...),
	}

	var sb strings.Builder
	sb.WriteString("*Requested actions*\n")
	if len(req.ScopeSummary) > 0 {
		for _, line := range req.ScopeSummary {
			sb.WriteString("• " + esc(line) + "\n")
		}
	} else {
		for _, a := range req.Actions {
			mode := "auto-execute"
			if !a.AutoExecute {
				mode = "requires per-request approval"
			}
			fmt.Fprintf(&sb, "• %s _(%s)_\n", esc(display.FormatServiceAction(a.Service, a.Action)), mode)
		}
	}
	b = append(b, section(strings.TrimRight(sb.String(), "\n")))

	if len(req.PlannedCalls) > 0 {
		var pc strings.Builder
		pc.WriteString("*Planned calls*\n")
		for _, c := range req.PlannedCalls {
			fmt.Fprintf(&pc, "• %s — %s\n", esc(display.FormatServiceAction(c.Service, c.Action)), esc(c.Reason))
		}
		b = append(b, section(strings.TrimRight(pc.String(), "\n")))
	}

	if req.ExpiresIn != "" {
		b = append(b, contextBlock(fmt.Sprintf("Expires in %s", esc(req.ExpiresIn))))
	}
	return b
}

// ── Scope expansion ───────────────────────────────────────────────────────────

func scopeExpansionBlocks(req notify.ScopeExpansionRequest) []block {
	f := [][2]string{{"Agent", req.AgentName}}
	if req.RiskLevel != "" && req.RiskLevel != "unknown" {
		f = append(f, [2]string{"Risk", riskEmoji(req.RiskLevel) + " " + req.RiskLevel})
	}
	if req.Lifetime != "" {
		f = append(f, [2]string{"Lifetime", req.Lifetime})
	}
	f = append(f, [2]string{"Time", stamp()})

	b := []block{
		header("🔄 Scope Expansion Request"),
		section("*Task*\n" + esc(req.Purpose)),
		fields(f...),
	}

	if req.Reason != "" {
		b = append(b, section(fmt.Sprintf("*Agent's stated reason*\n>%s", esc(req.Reason))))
	}

	if s := addedToolLines(req.AddedTools); s != "" {
		b = append(b, section("*New tools*\n"+s))
	}
	if s := replacedToolLines(req.ReplacedTools); s != "" {
		b = append(b, section("*Updated tools*\n"+s))
	}
	if s := addedEgressLines(req.AddedEgress); s != "" {
		b = append(b, section("*New egress*\n"+s))
	}
	if s := replacedEgressLines(req.ReplacedEgress); s != "" {
		b = append(b, section("*Updated egress*\n"+s))
	}
	if s := addedCredLines(req.AddedCredentials); s != "" {
		b = append(b, section("*New credentials*\n"+s))
	}
	if s := replacedCredLines(req.ReplacedCredentials); s != "" {
		b = append(b, section("*Updated credentials*\n"+s))
	}
	return b
}

func addedToolLines(ts []notify.ExpansionTool) string {
	var sb strings.Builder
	for _, t := range ts {
		fmt.Fprintf(&sb, "• `%s` — %s _(%s)_\n", esc(t.ToolName), esc(t.Why), expansionToolDisposition(t))
	}
	return strings.TrimRight(sb.String(), "\n")
}

// replacedToolLines renders a was/now diff. Showing only the new `why` would
// hide what the reviewer is actually being asked to change.
func replacedToolLines(ts []notify.ReplacedExpansionTool) string {
	var sb strings.Builder
	for _, t := range ts {
		fmt.Fprintf(&sb, "• `%s` _(%s)_\n    was: %s\n    now: %s\n",
			esc(t.New.ToolName), expansionToolDisposition(t.New), esc(t.Prior.Why), esc(t.New.Why))
	}
	return strings.TrimRight(sb.String(), "\n")
}

func addedEgressLines(es []notify.ExpansionEgress) string {
	var sb strings.Builder
	for _, e := range es {
		fmt.Fprintf(&sb, "• `%s` — %s\n", esc(e.Host), esc(e.Why))
	}
	return strings.TrimRight(sb.String(), "\n")
}

func replacedEgressLines(es []notify.ReplacedExpansionEgress) string {
	var sb strings.Builder
	for _, e := range es {
		fmt.Fprintf(&sb, "• `%s`\n    was: %s\n    now: %s\n", esc(e.New.Host), esc(e.Prior.Why), esc(e.New.Why))
	}
	return strings.TrimRight(sb.String(), "\n")
}

func addedCredLines(cs []notify.ExpansionCredential) string {
	var sb strings.Builder
	for _, c := range cs {
		fmt.Fprintf(&sb, "• `%s` — %s\n", esc(credLabel(c)), esc(c.Why))
	}
	return strings.TrimRight(sb.String(), "\n")
}

func replacedCredLines(cs []notify.ReplacedExpansionCredential) string {
	var sb strings.Builder
	for _, c := range cs {
		fmt.Fprintf(&sb, "• `%s`\n    was: %s\n    now: %s\n", esc(credLabel(c.New)), esc(c.Prior.Why), esc(c.New.Why))
	}
	return strings.TrimRight(sb.String(), "\n")
}

func credLabel(c notify.ExpansionCredential) string {
	if c.VaultItemHandle != "" {
		return c.VaultItemHandle
	}
	return c.VaultItemID
}

// expansionToolDisposition mirrors the Telegram renderer: a wildcard-covered
// action must not show a per-call/auto pill, or the reviewer reads a
// "needs approval" badge on something the wildcard already auto-approved.
func expansionToolDisposition(t notify.ExpansionTool) string {
	if t.WildcardCovered {
		return "covered by existing wildcard"
	}
	if !t.GatewayAction {
		return "local tool"
	}
	if t.AutoExecute {
		return "auto-execute"
	}
	return "requires per-call approval"
}

// ── Connection ────────────────────────────────────────────────────────────────

func connectionBlocks(req notify.ConnectionRequest) []block {
	return []block{
		header("🔗 Agent Connection Request"),
		fields(
			[2]string{"Agent", req.AgentName},
			[2]string{"IP address", req.IPAddress},
			[2]string{"Time", stamp()},
		),
		contextBlock("Approve only if you recognise this agent and address."),
	}
}

func riskEmoji(level string) string {
	switch strings.ToLower(level) {
	case "critical":
		return ":rotating_light:"
	case "high":
		return ":red_circle:"
	case "medium":
		return ":large_orange_diamond:"
	case "low":
		return ":large_green_circle:"
	default:
		return ""
	}
}

// paramValue renders a parameter for display, matching the Telegram
// notifier's handling of nested values.
func paramValue(v any) string {
	switch t := v.(type) {
	case string:
		return truncate(t, 300)
	case nil:
		return ""
	default:
		return truncate(fmt.Sprintf("%v", t), 300)
	}
}
