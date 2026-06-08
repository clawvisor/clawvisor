// Package llmjudge is the LLM-backed implementation of
// scriptjudge.Judge. Lives separately from scriptjudge so the leaf
// Judge interface stays import-free for the policies chain — only
// the daemon construction sites (server.go, e2e harness) need to
// pull in internal/llm + pkg/config.
package llmjudge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/clawvisor/clawvisor/internal/llm"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/scriptjudge"
	"github.com/clawvisor/clawvisor/pkg/config"
)

// Judge calls the daemon's configured verification LLM to classify a
// tool_use that carries script-session signals. Mirrors the
// construction pattern of inspector.LLMClientValidator: read the live
// VerificationConfig per call, build a fresh llm.Client, send
// (system + user), parse the JSON verdict.
//
// On any failure (provider not configured, transport error, parse
// error) the judge returns an error so the caller can fall through to
// the next chain stage rather than acting on a half-baked verdict.
type Judge struct {
	ConfigFn func() config.VerificationConfig
	Logger   *slog.Logger
	// PromptOverride lets tests inject a deterministic prompt. Empty
	// uses Prompt. Mutable — PromptSHA recomputes on each call so a
	// late override doesn't desynchronize audit-row provenance from
	// the prompt actually sent.
	PromptOverride string
}

// New constructs a judge backed by the daemon's configured
// verification LLM. configFn is consulted on every call so live
// config edits or env-var overrides flow through without a daemon
// restart.
func New(configFn func() config.VerificationConfig, logger *slog.Logger) *Judge {
	if logger == nil {
		logger = slog.Default()
	}
	return &Judge{ConfigFn: configFn, Logger: logger}
}

// Judge implements scriptjudge.Judge by delegating classification to
// the configured verification LLM. Latency, prompt-SHA, and token
// usage are captured in the returned Verdict so audit rows can show
// judge invocation cost + provenance.
func (j *Judge) Judge(ctx context.Context, in scriptjudge.Input) (scriptjudge.Verdict, error) {
	if j == nil || j.ConfigFn == nil {
		return scriptjudge.Verdict{}, scriptjudge.ErrNotConfigured
	}
	cfg := j.ConfigFn()
	if !cfg.Enabled {
		return scriptjudge.Verdict{}, scriptjudge.ErrNotConfigured
	}
	if !providerConfigured(cfg.LLMProviderConfig) {
		return scriptjudge.Verdict{}, scriptjudge.ErrNotConfigured
	}

	prompt := j.PromptOverride
	if prompt == "" {
		prompt = Prompt
	}
	promptSHA := j.PromptSHA()

	client := llm.NewClient(cfg.LLMProviderConfig)
	messages := []llm.ChatMessage{
		{Role: "system", Content: prompt, CacheControl: true},
		{Role: "user", Content: userMessage(in)},
	}

	start := time.Now()
	raw, usage, err := client.CompleteWithUsage(ctx, messages)
	elapsed := time.Since(start).Milliseconds()
	// Even on error, populate forensic fields so the evaluator can
	// emit an audit row showing the attempt actually happened — the
	// judge_error fact is useless without latency + prompt SHA, and
	// the absence of these previously meant operators couldn't tell
	// whether the judge was being called at all.
	errVerdict := scriptjudge.Verdict{PromptSHA: promptSHA, LatencyMS: elapsed}
	if usage != nil {
		errVerdict.InputTokens = usage.InputTokens
		errVerdict.OutputTokens = usage.OutputTokens
	}
	if err != nil {
		j.Logger.WarnContext(ctx, "lite-proxy: script-session judge LLM call failed",
			"tool", in.ToolName, "err", err.Error(), "latency_ms", elapsed, "prompt_sha", promptSHA)
		// Wrap with %w so callers can still errors.Is for
		// context.DeadlineExceeded / context.Canceled. The audit
		// emitter applies redactSensitive() to the surfaced string
		// before persistence — protects against LLM clients folding
		// response bodies into errors without breaking error-chain
		// detection here.
		return errVerdict, fmt.Errorf("scriptjudge transport: %w", err)
	}

	verdict, err := parseJSON(raw)
	if err != nil {
		j.Logger.WarnContext(ctx, "lite-proxy: script-session judge response parse failed",
			"tool", in.ToolName, "err", err.Error(), "latency_ms", elapsed, "prompt_sha", promptSHA)
		return errVerdict, fmt.Errorf("scriptjudge parse: %w", err)
	}
	verdict.PromptSHA = promptSHA
	verdict.LatencyMS = elapsed
	if usage != nil {
		verdict.InputTokens = usage.InputTokens
		verdict.OutputTokens = usage.OutputTokens
	}
	return verdict, nil
}

// PromptSHA returns the SHA-256 of the prompt currently in use, for
// callers that want to stamp it before invocation (the per-verdict
// PromptSHA is always populated, but this accessor lets tests assert
// the value matches without invoking the LLM).
//
// Not memoized: PromptOverride is exported and can change between
// calls in tests; caching would silently desynchronize the audit-row
// provenance from the prompt actually sent. SHA-256 over a ~2KB
// prompt is microseconds — the per-call cost is negligible vs. the
// LLM round-trip we're about to perform.
func (j *Judge) PromptSHA() string {
	if j == nil {
		return ""
	}
	prompt := j.PromptOverride
	if prompt == "" {
		prompt = Prompt
	}
	return promptHash(prompt)
}

func promptHash(prompt string) string {
	sum := sha256.Sum256([]byte(prompt))
	return hex.EncodeToString(sum[:])
}

// Prompt is the system prompt for the use-time script-session judge.
// The prompt frames the judge's role as recognition (not policy)
// because:
//   - Scope (target host, methods, path prefixes, max_uses, why) was
//     already vetted at mint time by the intent verifier.
//   - The resolver enforces session scope mechanically on every
//     actual request that reaches it; if the call routes through the
//     resolver at all, scope checks happen regardless.
//
// The judge's only question is: "does this tool_use's execution
// intend to emit credentialed requests targeting the resolver mount?"
// — answered against the actual tool input shape, not against a
// literal-prefix template. That makes variable-ization, Write+Bash
// staging, and Python/Node wrappers all legitimate shapes the judge
// can recognize.
const Prompt = `You are a recognition classifier for a security proxy that mediates credentialed API calls.

CONTEXT
An agent minted a "script session" — a short-lived envelope that lets it issue many credentialed proxy calls under one approved scope. The session was already vetted at mint time: its target host, methods, path prefixes, and rationale were checked against the agent's approved task purpose.

You are seeing this tool_use because the deterministic recognizer (which looks at literal URL prefixes in curl args) couldn't confirm the call routes through the resolver mount. Common reasons: the agent stashed the URL or headers in shell variables, wrote the script to disk before running it, or used a Python/Node/curl-via-base64 wrapper.

YOUR JOB
Decide whether this tool_use intends to emit credentialed requests that target the resolver mount. The resolver enforces session scope mechanically on every actual HTTP request, so you do NOT need to verify host/method/path. Recognition only.

LEGITIMATE SHAPES (ALLOW)
- Shell variable holding the resolver URL: ` + "`B='<resolver-base>/...'; curl \"$B\"`" + `
- Shell variable holding the header value: ` + "`H='X-Clawvisor-Caller: Bearer cv-script-...'; curl ... -H \"$H\"`" + `
- Multi-statement loops, pipelines, while-read, xargs, parallel curls — all with the resolver URL appearing somewhere as a literal (in a variable assignment or directly).
- Write/Edit of a script file whose contents embed curls that target the resolver. The Write itself is local file I/O, not a credentialed request; the credentialed call happens when the script is later executed.
- Python/Node/Perl/Ruby wrappers whose code targets the resolver URL.
- Echo/printf piped into bash where the embedded shell text targets the resolver.

BLOCK SHAPES
- Curls that unambiguously target a non-resolver host (e.g. ` + "`curl https://gmail.googleapis.com/...`" + `, ` + "`curl https://api.github.com/...`" + `) carrying the autovault placeholder directly. The agent skipped the proxy.
- Required headers entirely missing (no X-Clawvisor-Target-Host, no X-Clawvisor-Caller carrying cv-script-…) AND no resolver URL — looks like an unrelated tool_use that happens to mention the cv-script substring in prose.
- Tool_uses that emit no HTTP request at all (pure echo with no follow-up, unrelated file manipulation, etc.).

OUTPUT
Return ONLY a JSON object with this exact shape:
{
  "verdict": "allow" | "block",
  "reason": "<one-sentence summary of the decision>",
  "agent_guidance": "<EMPTY when verdict=allow. When verdict=block, a specific, actionable instruction the agent can follow in its next attempt — name the exact defect with concrete fix. Examples: 'Your curl targets https://gmail.googleapis.com directly; replace it with <resolver-base>/gmail/v1/...' or 'Your tool_use carries the cv-script token in prose but doesn't actually issue any HTTP request; if you intended a session call, issue the curl(s) directly under this tool_use'.>"
}

No prose outside the JSON object.`

type jsonResponse struct {
	Verdict       string `json:"verdict"`
	Reason        string `json:"reason"`
	AgentGuidance string `json:"agent_guidance"`
}

func userMessage(in scriptjudge.Input) string {
	tokenPresent := in.CVScriptToken != ""
	// Strip concrete cv-script tokens and autovault placeholders
	// before crossing into the third-party LLM provider per
	// AGENTS.md "do not log credentials." The judge sees that the
	// pattern is present (via cv_script_token_present + the
	// redacted placeholders left in tool_input) but never the
	// actual values.
	return fmt.Sprintf("resolver_base_url: %q\ncv_script_token_present: %v\ntool_name: %q\ntool_input: %s",
		in.ResolverBaseURL, tokenPresent, in.ToolName, scriptjudge.RedactSensitive(string(in.ToolInput)))
}

// maxAgentGuidanceBytes caps the LLM-authored agent_guidance string
// before it flows into the agent-facing substitute (Reason) and audit
// row. Without a cap, a misbehaving or attacker-influenced model could
// return an arbitrarily large block of text the proxy then forwards
// to the harness verbatim. 800 bytes fits the longest natural
// guidance we've seen (URL-replacement + alternate-host advice) with
// headroom; longer text gets truncated with an explicit marker.
const maxAgentGuidanceBytes = 800

func parseJSON(raw string) (scriptjudge.Verdict, error) {
	text := strings.TrimSpace(raw)
	text = strings.TrimPrefix(text, "```json")
	text = strings.TrimPrefix(text, "```")
	text = strings.TrimSuffix(text, "```")
	text = strings.TrimSpace(text)
	if i := strings.IndexByte(text, '{'); i > 0 {
		text = text[i:]
	}
	if j := strings.LastIndexByte(text, '}'); j >= 0 && j < len(text)-1 {
		text = text[:j+1]
	}
	var parsed jsonResponse
	if err := json.Unmarshal([]byte(text), &parsed); err != nil {
		return scriptjudge.Verdict{}, fmt.Errorf("judge JSON: %w", err)
	}
	v := strings.ToLower(strings.TrimSpace(parsed.Verdict))
	switch v {
	case "allow":
		return scriptjudge.Verdict{Allow: true, Reason: truncate(parsed.Reason, maxAgentGuidanceBytes)}, nil
	case "block":
		return scriptjudge.Verdict{
			Allow:         false,
			Reason:        truncate(parsed.Reason, maxAgentGuidanceBytes),
			AgentGuidance: truncate(parsed.AgentGuidance, maxAgentGuidanceBytes),
		}, nil
	default:
		return scriptjudge.Verdict{}, fmt.Errorf("judge verdict %q not in {allow,block}", parsed.Verdict)
	}
}

// truncate clips s to at most maxBytes runes, appending an explicit
// marker so the agent (and audit consumers) can see the truncation
// happened. Boundary picked to avoid splitting a multi-byte rune.
func truncate(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	// Find the last rune boundary at or before maxBytes.
	cut := maxBytes
	for cut > 0 && (s[cut]&0xC0) == 0x80 {
		cut--
	}
	return s[:cut] + "…(truncated)"
}

// providerConfigured mirrors inspector.llmProviderConfigured —
// API-key providers need a key, Vertex/Gemini authenticates via ADC
// and only needs Project+Region.
func providerConfigured(cfg config.LLMProviderConfig) bool {
	provider := strings.ToLower(strings.TrimSpace(cfg.Provider))
	switch provider {
	case "gemini", "vertex":
		return strings.TrimSpace(cfg.Project) != ""
	default:
		return strings.TrimSpace(cfg.APIKey) != ""
	}
}
