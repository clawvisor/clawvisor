package drivers

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	claudeBinary             = "claude"
	taskApprovalPromptMark   = "Clawvisor wants to create a task"
	toolUseBlockSingleMark   = "Clawvisor paused this tool call for approval"
	toolUseBlockTurnMark     = "Clawvisor paused this turn for approval"
)

// approvalPromptKind classifies an assistant-text turn as one of the
// lite-proxy's substituted approval prompts, or returns "" if the
// text is the agent's final answer for the step.
//
//   - "task_approval"  → "Clawvisor wants to create a task" — inline
//     task intercept; the approver decides yes/no.
//   - "tool_use_block" → "Clawvisor paused this tool call/turn for
//     approval" — the agent ran a tool without scope; the approver
//     decides whether to escalate ("task"), approve once ("yes"), or
//     deny ("no").
func approvalPromptKind(text string) string {
	switch {
	case strings.Contains(text, taskApprovalPromptMark):
		return "task_approval"
	case strings.Contains(text, toolUseBlockSingleMark),
		strings.Contains(text, toolUseBlockTurnMark):
		return "tool_use_block"
	}
	return ""
}

// approverFor returns the configured approver or the default
// always-approve / always-escalate fallback when none is set.
func approverFor(cfg Config) Approver {
	if cfg.Approver != nil {
		return cfg.Approver
	}
	return DefaultApprover{}
}

// Claude is the driver for the `claude` CLI (Claude Code).
type Claude struct{}

func NewClaude() *Claude { return &Claude{} }

func (Claude) Name() string { return "claude" }

func (Claude) Available() (bool, string) {
	if _, err := exec.LookPath(claudeBinary); err != nil {
		return false, "claude binary not found in PATH"
	}
	return true, ""
}

func (Claude) Start(_ context.Context, cfg Config) (Session, error) {
	if ok, why := (Claude{}).Available(); !ok {
		return nil, ErrSkip{Reason: why}
	}
	if cfg.LiteProxyURL == "" || cfg.AgentToken == "" || cfg.Workspace == "" {
		return nil, fmt.Errorf("claude: cfg.LiteProxyURL, AgentToken, Workspace are required")
	}
	model := cfg.Model
	if model == "" {
		// --bare loads no settings, so without --model claude falls back
		// to Haiku, which is the worst choice for an agentic scenario
		// (and gets 529'd more often). Pin to sonnet — same default as
		// the rest of the codebase, e.g. roles/anthropic.go.
		model = "claude-sonnet-4-6"
	}
	return &claudeSession{
		cfg:       cfg,
		sessionID: uuid.NewString(),
		model:     model,
	}, nil
}

type claudeSession struct {
	cfg       Config
	sessionID string
	model     string
	turnIndex int
}

func (s *claudeSession) Close() error { return nil }

func (s *claudeSession) Send(ctx context.Context, message string) (*StepOutcome, error) {
	// Each Send is itself a multi-turn agent loop until we get an
	// assistant-text turn that is NOT an inline-approval prompt.
	// Approval prompts get auto-replied with "yes" via --resume.
	outcome := &StepOutcome{}
	current := message
	start := time.Now()
	const innerCap = 6 // upper bound on approval round-trips per step
	for inner := range innerCap {
		args := s.baseArgs()
		// First call uses --session-id to pin the id we generate; later
		// calls switch to --resume so claude continues the same session.
		if s.turnIndex == 0 {
			args = append(args, "--session-id", s.sessionID)
		} else {
			args = append(args, "--resume", s.sessionID)
		}
		args = append(args, "-p", current)
		s.turnIndex++
		stdout, runErr := s.run(ctx, args)
		outcome.RawOutput += stdout
		final, toolCalls, resultErr := parseClaudeStreamJSON(stdout)
		outcome.ToolCallCount += toolCalls
		logf(s.cfg.Logf, "[claude] turn=%d toolCalls=%d resultErr=%q final=%q",
			inner, toolCalls, resultErr, quoteShortPrefix(final, 160))
		// claude exits non-zero on Anthropic 5xx / overload. The result
		// event in stdout has the error string. Bubble that up so the
		// test surfaces upstream-overload as the cause rather than a
		// generic Go exit-status-1.
		if runErr != nil && resultErr != "" {
			return outcome, fmt.Errorf("claude turn %d: upstream error: %s", inner, resultErr)
		}
		if runErr != nil {
			return outcome, fmt.Errorf("claude turn %d: %w", inner, runErr)
		}
		if kind := approvalPromptKind(final); kind != "" {
			reply, outcomeLabel := approverFor(s.cfg).Reply(kind, final)
			if reply == "" {
				return outcome, fmt.Errorf("claude turn %d: approver returned empty reply for %s", inner, kind)
			}
			switch kind {
			case "task_approval":
				switch outcomeLabel {
				case "approve":
					outcome.TaskApprovalPromptsApproved++
				case "deny":
					outcome.TaskApprovalPromptsDenied++
				}
			case "tool_use_block":
				outcome.ToolUseBlocksSeen++
			}
			current = reply
			continue
		}
		outcome.FinalText = final
		break
	}
	outcome.DurationMs = time.Since(start).Milliseconds()
	return outcome, nil
}

func (s *claudeSession) baseArgs() []string {
	args := []string{
		"--output-format", "stream-json",
		"--input-format", "text",
		"--add-dir", s.cfg.Workspace,
		"--dangerously-skip-permissions",
		"--bare",
		"--verbose", // required by stream-json on some claude versions
		"--exclude-dynamic-system-prompt-sections",
		// Don't pass --no-session-persistence: that flag prevents
		// sessions from being written to disk, which makes --resume
		// fail with "No conversation found with session ID".
		// Sessions land in $HOME/.claude/projects/... and get
		// cleaned up with the tempdir at test end (claude keys by
		// project path).
	}
	if s.model != "" {
		args = append(args, "--model", s.model)
	}
	return args
}

func (s *claudeSession) run(ctx context.Context, args []string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(cctx, claudeBinary, args...)
	cmd.Dir = s.cfg.Workspace
	cmd.Env = append(cmd.Environ(),
		"ANTHROPIC_BASE_URL="+s.cfg.LiteProxyURL,
		"ANTHROPIC_API_KEY="+s.cfg.AgentToken,
		// Don't bleed the user's local Claude Code creds into the test.
		"CLAUDE_CODE_SIMPLE=1",
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	if runErr != nil {
		logf(s.cfg.Logf, "[claude] exit error: %v stderr=%q stdout-tail=%q",
			runErr, stderr.String(), tailFor(stdout.String(), 1200))
		return stdout.String(), fmt.Errorf("claude run: %w (stderr: %s)", runErr, stderr.String())
	}
	return stdout.String(), nil
}

func tailFor(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}

// parseClaudeStreamJSON walks the stream-json output and extracts the
// final assistant text plus the tool_use count. Returns (final,
// toolCalls, resultErr) — resultErr is the message inside a terminal
// `{"type":"result","is_error":true,...}` event, used to surface
// upstream API failures (529 overload, etc.) with a meaningful cause
// instead of "claude exited 1".
func parseClaudeStreamJSON(out string) (final string, toolCalls int, resultErr string) {
	scanner := bufio.NewScanner(strings.NewReader(out))
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	var lastText string
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || line[0] != '{' {
			continue
		}
		var evt map[string]any
		if err := json.Unmarshal([]byte(line), &evt); err != nil {
			continue
		}
		eventType, _ := evt["type"].(string)
		switch eventType {
		case "assistant":
			msg, _ := evt["message"].(map[string]any)
			content, _ := msg["content"].([]any)
			for _, blk := range content {
				b, _ := blk.(map[string]any)
				bt, _ := b["type"].(string)
				switch bt {
				case "text":
					if t, ok := b["text"].(string); ok && t != "" {
						lastText = t
					}
				case "tool_use":
					toolCalls++
				}
			}
		case "result":
			if isErr, _ := evt["is_error"].(bool); isErr {
				if r, ok := evt["result"].(string); ok && r != "" {
					resultErr = r
				}
			}
		}
	}
	return lastText, toolCalls, resultErr
}
