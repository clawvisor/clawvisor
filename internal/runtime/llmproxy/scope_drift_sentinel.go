package llmproxy

import (
	"fmt"
	"strings"
)

// ScopeDriftPlaceholderToolName names the canonical placeholder tool
// the response rewriter substitutes in for a blocked tool_use. The
// harness's local execution of the placeholder is a no-op (`:` exits
// 0); the operator-facing comment names the original call so the
// transcript still describes what was blocked.
//
// The LLM never sees this placeholder — the inbound rewriter restores
// the model's original tool_use byte-for-byte before forwarding the
// next /v1/messages upstream.
const ScopeDriftPlaceholderToolName = "Bash"

// ScopeDriftPlaceholderMarker is the literal substring that identifies
// a Bash tool_use as a Clawvisor-injected placeholder in the
// transcript. Operator-facing; carries no semantic load on the wire
// (the inbound rewriter relies on the pending-substitution registry,
// not string matching on the placeholder).
const ScopeDriftPlaceholderMarker = "CLAWVISOR_BLOCKED"

// BuildScopeDriftPlaceholderCommand renders the Bash `command` string
// the placeholder tool_use carries. Shape:
//
//	: # CLAWVISOR_BLOCKED <originalName>(...) blocked — see Clawvisor audit / upstream menu
//
// The leading `:` is the POSIX no-op so the harness's local execution
// exits 0 with no side effect. The remainder is a shell comment, so the
// harness shows the operator a human-readable line in the transcript
// instead of an opaque encoded blob.
func BuildScopeDriftPlaceholderCommand(originalName, driftID string) string {
	name := sanitizePlaceholderField(originalName, "(unknown)")
	drift := strings.TrimSpace(driftID)
	if drift == "" {
		drift = "(none)"
	}
	return fmt.Sprintf(": # %s drift=%s tool=%s — blocked; the upstream model received the recovery menu in this call's tool_result.", ScopeDriftPlaceholderMarker, drift, name)
}

// BuildRecoverableDenyPlaceholderCommand renders the harness-safe
// placeholder for a recoverable-deny block — the proxy denied the call
// for a construction error the agent can fix on its next attempt
// (malformed control body, boundary check failure, inspector parse
// error, etc.). Shape mirrors BuildScopeDriftPlaceholderCommand so the
// harness's local execution is a no-op; the operator-facing comment
// names the reason instead of a drift_id.
func BuildRecoverableDenyPlaceholderCommand(originalName, reason string) string {
	name := sanitizePlaceholderField(originalName, "(unknown)")
	reasonLine := sanitizePlaceholderField(reason, "(no reason supplied)")
	return fmt.Sprintf(": # %s tool=%s — recoverable deny; the upstream model received the reason in this call's tool_result. Reason: %s", ScopeDriftPlaceholderMarker, name, reasonLine)
}

// AutoApprovePlaceholderMarker tags the auto-approve flavour of the
// placeholder so operators grepping the transcript can distinguish
// gate-bypassed task creations from blocked calls. Same Bash no-op
// mechanics underneath.
const AutoApprovePlaceholderMarker = "CLAWVISOR_AUTO_APPROVED"

// BuildAutoApprovePlaceholderCommand renders the harness-safe
// placeholder for an inline-task auto-approve interception — the
// proxy created the task synchronously and the upstream model will
// receive the task's augmentation context as the tool_result on the
// next inbound. The operator-facing comment names the task ID and
// purpose so the transcript still explains what just happened.
func BuildAutoApprovePlaceholderCommand(originalName, taskID, purpose string) string {
	name := sanitizePlaceholderField(originalName, "(unknown)")
	id := strings.TrimSpace(taskID)
	if id == "" {
		id = "(unknown)"
	}
	purposeLine := sanitizePlaceholderField(purpose, "(no purpose recorded)")
	return fmt.Sprintf(": # %s task=%s tool=%s — Clawvisor auto-approved this task creation (%s). The upstream model received the task augmentation context in this call's tool_result.", AutoApprovePlaceholderMarker, id, name, purposeLine)
}

func sanitizePlaceholderField(value, fallback string) string {
	v := strings.TrimSpace(value)
	if v == "" {
		return fallback
	}
	v = strings.ReplaceAll(v, "\n", " ")
	v = strings.ReplaceAll(v, "\r", " ")
	v = strings.ReplaceAll(v, "`", "'")
	return v
}
