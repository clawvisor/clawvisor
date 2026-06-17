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
	name := strings.TrimSpace(originalName)
	if name == "" {
		name = "(unknown)"
	}
	name = strings.ReplaceAll(name, "\n", " ")
	name = strings.ReplaceAll(name, "\r", " ")
	name = strings.ReplaceAll(name, "`", "'")
	drift := strings.TrimSpace(driftID)
	if drift == "" {
		drift = "(none)"
	}
	return fmt.Sprintf(": # %s drift=%s tool=%s — blocked; the upstream model received the recovery menu in this call's tool_result.", ScopeDriftPlaceholderMarker, drift, name)
}
