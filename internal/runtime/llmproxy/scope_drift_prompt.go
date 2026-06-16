package llmproxy

import (
	"fmt"
	"strings"
)

// renderScopeDriftMenu builds the three-option menu the agent sees when
// a tool call falls outside the active task's scope. The output is
// plain text — the proxy substitutes it as the tool_result content of a
// continuation, so the agent sees a continuation of the same
// conversation rather than an opaque error.
//
// Options (a) expand and (b) new_task reuse existing control-plane
// endpoints: the agent just makes a normal POST /control/tasks{,/expand}
// tool call. Option (c) one_off is emitted as a <clawvisor:decision>
// markup block in the agent's assistant text — the SSE postproc
// resolver parses that markup server-side, so no new agent-facing HTTP
// surface is required. The markup carries the drift_id verbatim so the
// proxy can match the decision to the original block.
//
// There is also an implicit fourth path: the agent picks none of the
// above and just emits its next turn normally. The menu names this in
// prose; the drift TTL-expires unresolved.
//
// The one-shot cap is enforced by the registry (ClaimOption refuses a
// second claim against the same drift_id).
//
// menu fields are the renderer's only inputs. controlBaseURL is the
// synthetic Clawvisor control host (passed in by the caller — see
// RoutingContext.ControlBaseURL). Empty controlBaseURL falls back to a
// path-only render so the agent at least gets the route; the caller
// should always supply a base URL in production.
func renderScopeDriftMenu(menu MenuFields, controlBaseURL string) string {
	if menu.DriftID == "" {
		// Defensive: a drift with no ID is a bug, not something to
		// render. Fall back to a single-option message rather than
		// emitting a menu the agent can't actually use.
		return "Clawvisor: this tool call is outside your active task scope, and no drift record was minted. Create a new task or expand the active task and retry."
	}

	base := strings.TrimRight(controlBaseURL, "/")

	service := strings.TrimSpace(menu.Service)
	action := strings.TrimSpace(menu.Action)
	target := service
	if action != "" {
		if target == "" {
			target = action
		} else {
			target = service + "." + action
		}
	}
	if target == "" {
		target = "this tool call"
	}

	reason := strings.TrimSpace(menu.ReasonText)
	if reason == "" {
		reason = "no explanation supplied"
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Clawvisor: %s is outside your current task scope.\n", sanitizeUserText(target))
	fmt.Fprintf(&b, "  Reason: %s\n", sanitizeUserText(reason))
	if src := strings.TrimSpace(string(menu.Source)); src != "" {
		fmt.Fprintf(&b, "  Block source: %s\n", src)
	}
	fmt.Fprintf(&b, "  Drift ID: %s\n", menu.DriftID)

	b.WriteString("\nChoose ONE response. Each drift_id resolves at most once — once you commit to an option below, the proxy will not let you try another against this same drift_id.\n")

	// (a) Expand the active task. Only meaningful when a task was matched
	// at scope-check time. We still surface the option even when no task
	// matched (TaskID empty) because the agent may have other active
	// tasks it could expand; the endpoint validates the referenced
	// task_id and rejects malformed expansions.
	b.WriteString("\n(a) Expand the active task — same purpose continues, just add this action.\n")
	if menu.TaskID != "" {
		fmt.Fprintf(&b, "    POST %s/control/tasks/%s/expand?surface=inline\n", base, menu.TaskID)
		b.WriteString("    Body: {\"service\":\"" + sanitizeUserText(service) + "\",\"action\":\"" + sanitizeUserText(action) + "\",\"reason\":\"<why this action belongs in the existing task>\",\"drift_id\":\"" + menu.DriftID + "\"}\n")
	} else {
		b.WriteString("    No active task was matched at block time. Skip (a) and use (b) instead unless you mean to expand a different active task — list with GET /control/tasks first.\n")
	}

	// (b) Create a new task.
	b.WriteString("\n(b) Create a new task — genuinely different goal, bucket it separately.\n")
	fmt.Fprintf(&b, "    POST %s/control/tasks?surface=inline\n", base)
	b.WriteString("    Body: <task envelope as documented in /control/skill, plus \"drift_id\":\"" + menu.DriftID + "\">\n")

	// (c) One-off — markup, not an HTTP call. The agent's rationale is
	// shown to the user verbatim in the approval prompt.
	//
	// The example markup MUST sit inside a triple-backtick fence: the
	// SSE postproc's parseScopeDriftDecisions runs over the rewritten
	// response body which contains this menu (the rewriter substitutes
	// blocked tool_use → SubstituteWith → this menu text), and an
	// unfenced example would self-match — the parser would treat it as
	// a real agent decision, consume the one-shot cap, and splice in a
	// one-off approval prompt before the agent ever picks an option.
	// insideTripleFenceBlock recognizes line-leading ``` fences and
	// suppresses matches inside them; the agent's own emission won't
	// be fenced.
	fmt.Fprintf(&b, "\n(c) One-off — emit this in your assistant text (NOT inside a code block) to ask the user to authorize a single execution:\n")
	b.WriteString("```\n")
	fmt.Fprintf(&b, "<clawvisor:decision drift=%q option=\"one-off\">\n", menu.DriftID)
	b.WriteString("A one-sentence rationale shown verbatim to the user. Why is this throwaway?\n")
	b.WriteString("</clawvisor:decision>\n")
	b.WriteString("```\n")
	b.WriteString("    The user sees the original tool, the block reason, and your rationale, then approves or denies. On approve, Clawvisor pre-clears this single call. On deny, the drift is closed.\n")

	// Implicit fall-through.
	b.WriteString("\n(implicit) If none of the above fit — for example, the user clarified, the previous step returned what was needed, or the call was a mistake — just emit your next turn normally and do something else. Clawvisor will NOT retry the blocked call on its own; the drift expires unresolved. This is a valid path, not an error.\n")

	b.WriteString("\nOn a success signal from the proxy — task approved, or user-approved one-off — re-emit the original tool call unchanged. Clawvisor pre-clears it once on this drift_id.")

	return b.String()
}
