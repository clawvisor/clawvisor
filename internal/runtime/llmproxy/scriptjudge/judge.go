// Package scriptjudge is the leaf interface + value types for the
// use-time script-session classifier. Concrete implementations live
// in sub-packages (scriptjudge/llmjudge for the LLM-backed one) so
// importing the interface costs only stdlib — `policies` can depend
// on Judge without transitively pulling in internal/llm + pkg/config.
//
// A Judge fires only when scriptrecognition.Recognize returns
// URLUnrecognized (the agent clearly intended a script-session call
// but variable-ized the URL, Write-staged the script, or wrapped it
// in a non-curl language). The Judge's job is recognition, not
// policy: scope was vetted at mint time by the intent verifier; the
// resolver enforces scope on every actual request. The Judge just
// decides whether the tool_use intends to route through the resolver,
// and on a block returns specific actionable guidance the agent can
// use for the next try.
package scriptjudge

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"

	"github.com/clawvisor/clawvisor/internal/runtime/placeholdershape"
)

// Verdict is the outcome of asking a Judge to classify a tool_use.
type Verdict struct {
	// Allow reports whether the tool_use should pass through to the
	// resolver. The resolver enforces session scope on every actual
	// request, so the Judge's job is recognition (does this call
	// intend to route through the resolver?), not policy.
	Allow bool

	// Reason is a one-sentence summary of the decision, audit-only.
	Reason string

	// AgentGuidance is the actionable continuation message sent back
	// to the agent when Allow=false. It should name the specific
	// defect ("your curl targets gmail.googleapis.com directly;
	// replace with http://localhost:25297/api/proxy/...") rather
	// than restating policy.
	AgentGuidance string

	// Forensics — populated by LLM-backed implementations so audit
	// rows can surface judge invocation cost + provenance. Empty/zero
	// on Noop or any judge that doesn't track these.

	// PromptSHA is the SHA-256 of the system prompt the judge used.
	// Pinned in audit rows so prompt edits are forensically visible.
	PromptSHA string

	// LatencyMS is the wall-clock time the judge call took, including
	// transport. Captured by the implementation, not the caller, so
	// retries/hedging are accounted for.
	LatencyMS int64

	// InputTokens / OutputTokens are the LLM's usage counts. Zero
	// when the provider doesn't report them or when the judge doesn't
	// call an LLM (Noop).
	InputTokens  int
	OutputTokens int
}

// Judge classifies a tool_use that carries script-session signals but
// slipped past the deterministic recognizer. A nil judge or one that
// returns an error is treated by callers as "no verdict available" —
// the call falls through to whatever the chain's next stage decides
// (typically the inspector's generic refusal).
type Judge interface {
	Judge(ctx context.Context, input Input) (Verdict, error)
}

// Input is everything the judge needs to classify a tool_use.
// ResolverBaseURL anchors what "correctly routed" looks like; the
// cv-script token surfaced from the tool_use lets the judge confirm
// the agent isn't mentioning the prefix in unrelated prose.
type Input struct {
	ToolName        string
	ToolInput       json.RawMessage
	ResolverBaseURL string
	// CVScriptToken is the first `cv-script-…` literal extracted from
	// the tool_use input. Empty when none was found (the caller
	// should not invoke the judge in that case).
	CVScriptToken string
}

// ErrNotConfigured signals "no judge available." Callers should treat
// it as "fall through to the next chain stage," not as a block.
var ErrNotConfigured = errors.New("scriptjudge: not configured")

// tokenBodyPattern is the single source of truth for the
// cv-script-<body> shape. TokenRE (unanchored) and tokenExactRE
// (anchored to full string) are both derived from it so they can't
// drift apart. The token alphabet matches what MintScriptSession
// produces (base32-like lowercase + digits); kept permissive so
// future encoding changes don't silently drop matches.
const tokenBodyPattern = `cv-script-[a-z0-9]+`

// TokenRE matches a `cv-script-…` token anywhere in a string.
//
// Single canonical pattern shared by ExtractToken (this package) and
// the scriptrecognition substring fallback. Don't redefine
// elsewhere — the two had drifted before consolidation.
var TokenRE = regexp.MustCompile(tokenBodyPattern)

// tokenExactRE is TokenRE anchored to the full string. Used for
// header-value checks where the value as a whole must BE a token
// (after Bearer stripping), not just contain one — without anchoring,
// shapes like `Bearer token=cv-script-abc` would falsely match.
var tokenExactRE = regexp.MustCompile(`^` + tokenBodyPattern + `$`)

// ExtractToken pulls the first cv-script-prefixed literal out of
// arbitrary text. Returns "" when none is present. Used by the
// evaluator to surface a token from a tool_use whose header layout the
// AST recognizer can't see (variable-ized, file-staged, etc.).
func ExtractToken(s string) string {
	return TokenRE.FindString(s)
}

// HasToken reports whether s contains a complete cv-script-<body>
// token somewhere. Use this when scanning arbitrary text (command
// bodies, file contents, marshaled tool_use input). For header-value
// checks where the entire trimmed value should BE a token, use
// IsToken instead.
func HasToken(s string) bool {
	return TokenRE.MatchString(s)
}

// IsToken reports whether s is EXACTLY a cv-script-<body> token —
// no leading or trailing characters. Use this for caller-header
// validation; HasToken would accept malformed shapes like
// `token=cv-script-abc` that the resolver middleware later rejects.
func IsToken(s string) bool {
	return tokenExactRE.MatchString(s)
}

// RedactSensitive strips concrete cv-script tokens and autovault
// placeholders from a string before it leaves the proxy boundary for
// the external LLM judge — or before persistence in an audit row
// where a wrapped LLM-client error might have folded in a response
// body. Tokens become `cv-script-<redacted>` and placeholders become
// `autovault_<redacted>` so consumers can see WHERE the credential
// signals appear without receiving the values themselves.
//
// Reuses placeholdershape.AutovaultRE rather than redefining the
// pattern — the whole point of placeholdershape is one canonical
// home for the autovault shape.
func RedactSensitive(s string) string {
	s = TokenRE.ReplaceAllString(s, "cv-script-<redacted>")
	s = placeholdershape.AutovaultRE.ReplaceAllString(s, "autovault_<redacted>")
	return s
}

// RedactJudgeError is an alias for RedactSensitive intended for the
// audit-row code path so the call site reads as the intent (strip
// credentials from a judge error before persistence).
func RedactJudgeError(s string) string {
	return RedactSensitive(s)
}

// Noop always declines to classify. Used in tests and configurations
// where no LLM is wired.
type Noop struct{}

// Judge implements Judge for the no-op case.
func (Noop) Judge(_ context.Context, _ Input) (Verdict, error) {
	return Verdict{}, ErrNotConfigured
}
