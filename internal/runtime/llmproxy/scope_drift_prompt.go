package llmproxy

import (
	"fmt"
	"strings"
)

// renderScopeDriftMenu builds the four-option menu the agent sees when a
// tool call falls outside the active task's scope. The output is plain
// text — the harness substitutes it as the tool_use result, so the agent
// sees a continuation of the same conversation rather than an opaque
// error.
//
// Options (a) and (b) reuse existing control-plane endpoints: the agent
// just makes a normal POST /control/tasks{,/expand} tool call. Options
// (c) and (d) are emitted as <clawvisor:decision> markup in the agent's
// assistant text — the lite-proxy resolver parses that markup
// server-side, so no new agent-facing HTTP surface is required. The
// markup is required to carry the drift_id verbatim so the proxy can
// match the decision to the original block.
//
// The one-shot cap is enforced by the registry (ClaimOption refuses a
// second claim against the same drift_id).
//
// menu fields are the renderer's only inputs. controlBaseURL is the
// synthetic Clawvisor control host (https://clawvisor.local under the
// proxy-lite intercept); we render the full URL for (a)/(b) so the
// agent doesn't have to assemble it from path fragments.
func renderScopeDriftMenu(menu MenuFields, controlBaseURL string) string {
	if menu.DriftID == "" {
		// Defensive: a drift with no ID is a bug, not something to
		// render. Fall back to a single-option message rather than
		// emitting a menu the agent can't actually use.
		return "Clawvisor: this tool call is outside your active task scope, and no drift record was minted. Create a new task or expand the active task and retry."
	}

	base := strings.TrimRight(controlBaseURL, "/")
	if base == "" {
		base = "https://clawvisor.local"
	}

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

	b.WriteString("\nChoose ONE response. Each drift_id resolves exactly once — once you commit to an option below, the proxy will not let you try another against this same drift_id.\n")

	// (a) Expand the active task. Only meaningful when a task was matched
	// at scope-check time. We still surface the option even on task_scope
	// blocks where no task matched (TaskID empty) because the agent may
	// have other active tasks it could expand; the endpoint validates the
	// referenced task_id and rejects malformed expansions.
	b.WriteString("\n(a) Expand the active task — same purpose continues, just add this action.\n")
	if menu.TaskID != "" {
		fmt.Fprintf(&b, "    POST %s/control/tasks/%s/expand\n", base, menu.TaskID)
		b.WriteString("    Body: {\"service\":\"" + sanitizeUserText(service) + "\",\"action\":\"" + sanitizeUserText(action) + "\",\"reason\":\"<why this action belongs in the existing task>\"}\n")
	} else {
		b.WriteString("    No active task was matched at block time. Skip (a) and use (b) instead unless you mean to expand a different active task — list with GET /control/tasks first.\n")
	}

	// (b) Create a new task.
	b.WriteString("\n(b) Create a new task — genuinely different goal, bucket it separately.\n")
	fmt.Fprintf(&b, "    POST %s/control/tasks?surface=inline\n", base)
	b.WriteString("    Body: <task envelope as documented in /control/skill>\n")

	// (c) One-off — markup, not an HTTP call. The agent's rationale
	// is shown to the user verbatim in the approval prompt.
	fmt.Fprintf(&b, "\n(c) One-off — emit this in your assistant text to ask the user to authorize a single execution:\n")
	fmt.Fprintf(&b, "    <clawvisor:decision drift=%q option=\"one-off\">\n", menu.DriftID)
	b.WriteString("    A one-sentence rationale shown verbatim to the user. Why is this throwaway?\n")
	b.WriteString("    </clawvisor:decision>\n")
	b.WriteString("    The user sees the original tool, the block reason, and your rationale, then approves or denies. On approve, Clawvisor pre-clears this single call. On deny, the drift is closed.\n")

	// (d) False-positive justification — markup, not an HTTP call.
	fmt.Fprintf(&b, "\n(d) False positive — emit this in your assistant text to ask the verifier to reconsider:\n")
	fmt.Fprintf(&b, "    <clawvisor:decision drift=%q option=\"justify\">\n", menu.DriftID)
	b.WriteString("    Articulate the concrete connection between this call and the active task purpose. Confident assertion alone will be rejected.\n")
	b.WriteString("    </clawvisor:decision>\n")
	b.WriteString("    The same verifier re-evaluates with your justification threaded in. If it accepts, the call is pre-cleared and you can retry. If it rejects, this drift_id is closed — pick option (a) or (b) on a fresh retry of the original tool call.\n")

	b.WriteString("\nWhen you receive a success signal from the proxy — task approved, verifier re-accepted, or user-approved one-off — re-emit the original tool call unchanged. Clawvisor pre-clears it once on this drift_id.")

	return b.String()
}
