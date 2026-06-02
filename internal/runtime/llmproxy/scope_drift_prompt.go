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
// The four options are intentionally LABELLED rather than numbered so a
// careless free-text reply ("1") doesn't accidentally resolve a drift;
// the agent must emit a structured POST to the corresponding endpoint
// to claim the option. The one-shot cap is enforced by the registry
// (ClaimOption refuses a second claim against the same drift_id).
//
// menu fields are the renderer's only inputs. controlBaseURL is the
// synthetic Clawvisor control host (https://clawvisor.local under the
// proxy-lite intercept); we render the full URL so the agent doesn't
// have to assemble it from path fragments.
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

	b.WriteString("\nChoose ONE response. You do not get to retry against another option — once you claim an option for this drift_id, no further claims succeed.\n")

	// (a) Expand the active task. Only meaningful when a task was matched
	// at scope-check time. We still surface the option even on task_scope
	// blocks where no task matched (TaskID empty) because the agent may
	// have other active tasks it could expand; the endpoint validates the
	// referenced task_id and rejects malformed expansions.
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
	b.WriteString("    Body: <task envelope> + {\"drift_id\":\"" + menu.DriftID + "\"}\n")

	// (d) False-positive justification. (Option (c) one-off is documented
	// in the schema but not surfaced here on this build — the user-
	// approval channel that flips its outcome to succeeded isn't wired
	// yet, so claiming it would burn the one-shot cap on a path that
	// can't complete. /control/scope-drift/{id}/one-off returns 501
	// with a redirection message if the agent calls it directly.)
	b.WriteString("\n(d) False positive — argue the verifier was wrong; same verifier re-evaluates.\n")
	fmt.Fprintf(&b, "    POST %s/control/scope-drift/%s/justify\n", base, menu.DriftID)
	b.WriteString("    Body: {\"justification\":\"<articulate the connection between this call and the active task purpose; confident assertion is not enough>\"}\n")

	b.WriteString("\nWhen you choose (a)/(b) and the user approves, re-emit the original tool call — Clawvisor pre-clears it once on this drift_id. When you choose (d) and the verifier re-accepts, do the same. If your chosen option is denied, do NOT re-attempt under this drift_id.")

	return b.String()
}
