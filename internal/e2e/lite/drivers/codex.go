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
)

const codexBinary = "codex"
const defaultCodexModel = "gpt-5.5"

// Codex is the driver for the `codex` CLI (OpenAI Codex).
type Codex struct{}

func NewCodex() *Codex { return &Codex{} }

func (Codex) Name() string { return "codex" }

func (Codex) Available() (bool, string) {
	if _, err := exec.LookPath(codexBinary); err != nil {
		return false, "codex binary not found in PATH"
	}
	return true, ""
}

func (Codex) Start(_ context.Context, cfg Config) (Session, error) {
	if ok, why := (Codex{}).Available(); !ok {
		return nil, ErrSkip{Reason: why}
	}
	if cfg.LiteProxyURL == "" || cfg.AgentToken == "" || cfg.Workspace == "" {
		return nil, fmt.Errorf("codex: cfg.LiteProxyURL, AgentToken, Workspace are required")
	}
	model := cfg.Model
	if model == "" {
		model = defaultCodexModel
	}
	return &codexSession{cfg: cfg, model: model}, nil
}

type codexSession struct {
	cfg       Config
	model     string
	sessionID string // captured from the first invocation's event stream
}

func (s *codexSession) Close() error { return nil }

func (s *codexSession) Send(ctx context.Context, message string) (*StepOutcome, error) {
	outcome := &StepOutcome{}
	current := message
	start := time.Now()
	innerCap := s.cfg.MaxTurnsPerStep
	if innerCap <= 0 {
		innerCap = 6
	}
	for inner := range innerCap {
		args, captureSessionID, err := s.buildArgs(current)
		if err != nil {
			return outcome, err
		}
		stdout, err := s.run(ctx, args)
		outcome.RawOutput += stdout
		if err != nil {
			return outcome, fmt.Errorf("codex turn %d: %w", inner, err)
		}
		parsed := parseCodexJSONL(stdout)
		outcome.ToolCallCount += parsed.toolCalls
		if captureSessionID && parsed.sessionID != "" {
			s.sessionID = parsed.sessionID
		}
		logf(s.cfg.Logf, "[codex] turn=%d sessionID=%s toolCalls=%d final=%q",
			inner, s.sessionID, parsed.toolCalls, quoteShortPrefix(parsed.finalText, 160))
		if kind := approvalPromptKind(parsed.finalText); kind != "" {
			reply, outcomeLabel := approverFor(s.cfg).Reply(kind, parsed.finalText)
			if reply == "" {
				return outcome, fmt.Errorf("codex turn %d: approver returned empty reply for %s", inner, kind)
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
			case "tool_use_hard_block":
				outcome.ToolUseBlocksSeen++
			}
			current = reply
			continue
		}
		outcome.FinalText = parsed.finalText
		outcome.DurationMs = time.Since(start).Milliseconds()
		return outcome, nil
	}
	outcome.DurationMs = time.Since(start).Milliseconds()
	return outcome, fmt.Errorf("codex: step exceeded MaxTurnsPerStep=%d without reaching a final text turn (stuck in approval loop?)", innerCap)
}

// buildArgs picks between `codex exec ...` (first call) and
// `codex exec resume <id> ...` (follow-up). Returns whether the run is
// expected to mint a fresh session id we should capture.
//
// `codex exec resume` does NOT accept -C / --add-dir — those are
// exec-only flags. The resume command accepts -m, and the e2e harness
// passes it on every turn so Codex does not fall back to a CLI default
// model that may be unsupported for the configured auth path.
func (s *codexSession) buildArgs(prompt string) (args []string, capture bool, err error) {
	cfgFlags := s.providerConfigFlags()
	// `codex exec resume` rejects `--color`, `-C`, `--add-dir`.
	// Keep the resume option set minimal and add the exec-only flags
	// separately for the first call.
	commonResume := []string{
		"--json",
		"--skip-git-repo-check",
		"--dangerously-bypass-approvals-and-sandbox",
		"--ignore-user-config",
	}
	commonResume = append(commonResume, cfgFlags...)
	if s.model != "" {
		commonResume = append(commonResume, "-m", s.model)
	}
	if s.sessionID != "" {
		args = append(args, "exec", "resume")
		args = append(args, commonResume...)
		args = append(args, s.sessionID, prompt)
		return args, false, nil
	}
	commonExec := append([]string{}, commonResume...)
	commonExec = append(commonExec, "-C", s.cfg.Workspace, "--color", "never")
	args = append(args, "exec")
	args = append(args, commonExec...)
	args = append(args, prompt)
	return args, true, nil
}

func (s *codexSession) providerConfigFlags() []string {
	base := strings.TrimRight(s.cfg.LiteProxyURL, "/") + "/v1"
	return []string{
		"-c", `model_provider="clawvisor-test"`,
		"-c", `model_providers.clawvisor-test.name="ClawvisorTest"`,
		"-c", `model_providers.clawvisor-test.base_url="` + base + `"`,
		"-c", `model_providers.clawvisor-test.wire_api="responses"`,
		"-c", `model_providers.clawvisor-test.requires_openai_auth=true`,
		"-c", `model_providers.clawvisor-test.env_http_headers.X-Clawvisor-Agent-Token="CLAWVISOR_AGENT_TOKEN"`,
	}
}

func (s *codexSession) run(ctx context.Context, args []string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(cctx, codexBinary, args...)
	cmd.Dir = s.cfg.Workspace
	cmd.Env = append(cmd.Environ(),
		"CLAWVISOR_AGENT_TOKEN="+s.cfg.AgentToken,
		"OPENAI_API_KEY="+s.cfg.AgentToken,
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("codex run: %w (stderr: %s)", err, stderr.String())
	}
	return stdout.String(), nil
}

type codexParseResult struct {
	finalText string
	toolCalls int
	sessionID string
}

// parseCodexJSONL extracts the final assistant text, tool call count,
// and session id from the codex `--json` event stream.
//
// Empirically codex emits (codex-cli 0.130.0):
//   - {"type":"thread.started","thread_id":"<uuid>"} — capture as sessionID
//   - {"type":"turn.started"} / {"type":"turn.completed"}
//   - tool calls under various item.* shapes
//   - the agent's final text under "item.completed" or in "message" body
//
// The parser is tolerant — it'll pick up whatever shape the version
// of codex on the harness machine emits, falling back to substring
// matching on event type.
func parseCodexJSONL(out string) codexParseResult {
	var r codexParseResult
	scanner := bufio.NewScanner(strings.NewReader(out))
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || line[0] != '{' {
			continue
		}
		var evt map[string]any
		if err := json.Unmarshal([]byte(line), &evt); err != nil {
			continue
		}
		t, _ := evt["type"].(string)
		switch {
		case t == "thread.started", t == "session.created", t == "session_created":
			if id := codexFirstString(evt, "thread_id", "session_id", "id", "conversation_id"); id != "" {
				r.sessionID = id
			}
		case strings.Contains(t, "tool"), strings.Contains(t, "exec_command"), strings.Contains(t, "function"):
			if isStart := strings.Contains(t, "start") || strings.Contains(t, "begin") || t == "tool_call"; isStart {
				r.toolCalls++
			}
		case strings.Contains(t, "agent_message"), strings.Contains(t, "assistant"), strings.Contains(t, "item.completed"):
			if text := codexExtractText(evt); text != "" {
				r.finalText = text
			}
		}
	}
	return r
}

func codexFirstString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

// codexExtractText pulls a text payload out of a JSONL event. Tries the
// shapes we've observed across codex versions.
func codexExtractText(m map[string]any) string {
	if t, ok := m["text"].(string); ok && t != "" {
		return t
	}
	if t, ok := m["message"].(string); ok && t != "" {
		return t
	}
	if msg, ok := m["message"].(map[string]any); ok {
		if t, ok := msg["text"].(string); ok && t != "" {
			return t
		}
		if content, ok := msg["content"].([]any); ok {
			for _, c := range content {
				if cm, ok := c.(map[string]any); ok {
					if t, ok := cm["text"].(string); ok && t != "" {
						return t
					}
				}
			}
		}
	}
	if item, ok := m["item"].(map[string]any); ok {
		if t, ok := item["text"].(string); ok && t != "" {
			return t
		}
		if content, ok := item["content"].([]any); ok {
			for _, c := range content {
				if cm, ok := c.(map[string]any); ok {
					if t, ok := cm["text"].(string); ok && t != "" {
						return t
					}
				}
			}
		}
	}
	return ""
}
