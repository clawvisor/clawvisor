// Package drivers wraps the production CLI agents (claude, codex) so
// the lite-proxy e2e harness can drive them headlessly. Each driver
// implements the same Session.Send shape: deliver one user message,
// wait for the agent to stop emitting tool_uses for that turn, return
// the assistant's final text + counts.
//
// The drivers do NOT speak the LLM wire protocol themselves — they
// shell out to `claude` / `codex` with env vars and config flags that
// point the CLI at the harness's in-process lite-proxy HTTP server.
// This means the agent loop, prompt assembly, tool calls, and
// approval reply routing are all production code paths.
package drivers

import (
	"context"
	"fmt"
)

// Config is everything a driver needs to start a session.
type Config struct {
	// LiteProxyURL is the in-process httptest server URL the CLI
	// should point at instead of the real Anthropic / OpenAI API.
	LiteProxyURL string

	// AgentToken is the cvis_… bearer the CLI presents to the
	// lite-proxy. Each driver maps this to the env var its CLI reads
	// (ANTHROPIC_API_KEY for claude, CLAWVISOR_AGENT_TOKEN for codex).
	AgentToken string

	// Workspace is the per-scenario tempdir. The CLI's filesystem
	// tools are sandboxed to (or cd'd into) this path.
	Workspace string

	// Model is an optional model override (e.g. "claude-sonnet-4-6"
	// or "gpt-5.5"). Empty means use the driver default.
	Model string

	// Approver decides how to reply when the lite-proxy substitutes
	// an approval prompt into the assistant turn. nil-safe: when nil,
	// drivers default to "yes" for task-approval prompts and "task"
	// for tool-use-block prompts.
	Approver Approver

	// Logf is a per-step logger (typically t.Logf). nil-safe.
	Logf func(format string, args ...any)
}

// Approver decides how to reply to an approval prompt the lite-proxy
// substituted into an assistant turn. Kind is one of:
//   - "task_approval" — "Clawvisor wants to create a task…"
//   - "tool_use_block" — "Clawvisor paused this tool call/turn…"
//
// Reply must be one of:
//   - "yes" → approve
//   - "no"  → deny
//   - "task" → escalate a tool-use block into a task definition
//
// Drivers call this per prompt; the harness provides the scenario's
// scripted policy.
type Approver interface {
	Reply(kind string, prompt string) (reply string, outcome string)
}

// DefaultApprover always approves task creation and always escalates
// tool-use blocks to task definitions. Used when Config.Approver is nil.
type DefaultApprover struct{}

func (DefaultApprover) Reply(kind, _ string) (string, string) {
	switch kind {
	case "task_approval":
		return "yes", "approve"
	case "tool_use_block":
		return "task", "escalate"
	}
	return "", ""
}

// Driver creates Sessions for one agent CLI. Implementations should be
// stateless — Sessions hold all per-run state (session id, transcripts).
type Driver interface {
	Name() string
	Available() (bool, string) // (yes/no, reason if no)
	Start(ctx context.Context, cfg Config) (Session, error)
}

// Session is one logical conversation. Send delivers a single user
// message and returns the agent's response for that step.
type Session interface {
	Send(ctx context.Context, message string) (*StepOutcome, error)
	Close() error
}

// StepOutcome bundles what the harness needs after one Session.Send.
type StepOutcome struct {
	// FinalText is the assistant's last text block for this step.
	FinalText string
	// ToolCallCount counts how many tool_uses the agent emitted.
	ToolCallCount int
	// TaskApprovalPromptsApproved counts task-approval prompts the
	// approver replied "yes" to.
	TaskApprovalPromptsApproved int
	// TaskApprovalPromptsDenied counts task-approval prompts the
	// approver replied "no" to.
	TaskApprovalPromptsDenied int
	// ToolUseBlocksSeen counts how many times the lite-proxy
	// substituted "Clawvisor paused this tool call for approval"
	// into the assistant turn. The driver typically escalates these
	// via "task" — but the approver can choose deny instead.
	ToolUseBlocksSeen int
	// RawOutput is the raw CLI stdout for debugging.
	RawOutput string
	// DurationMs is the elapsed time for this step.
	DurationMs int64
}

// ErrSkip is returned by Start when the driver isn't usable in this
// environment (e.g. CLI binary missing, required env var absent).
type ErrSkip struct{ Reason string }

func (e ErrSkip) Error() string { return "driver skipped: " + e.Reason }

// IsSkip reports whether err is an ErrSkip.
func IsSkip(err error) bool {
	_, ok := err.(ErrSkip)
	return ok
}

// logf is a nil-safe wrapper.
func logf(fn func(string, ...any), format string, args ...any) {
	if fn != nil {
		fn(format, args...)
	}
}

// quoteShortPrefix returns a short single-line preview of s, useful
// for log lines.
func quoteShortPrefix(s string, n int) string {
	out := []rune{}
	for _, r := range s {
		if r == '\n' || r == '\r' {
			out = append(out, ' ')
			continue
		}
		out = append(out, r)
	}
	if len(out) <= n {
		return string(out)
	}
	return fmt.Sprintf("%s…", string(out[:n]))
}
