// Package inspector decides whether a tool_use emitted by the model — that
// carries an `autovault_…` shadow-placeholder — is a credentialed HTTP API
// call that should be rewritten to flow through the lite-proxy resolver.
//
// The pipeline is:
//
//  1. Substring trigger (existing autovault.LooksLikeShadow) — cheap pre-filter.
//  2. Deterministic structural parser — recognizes named tool shapes
//     (WebFetch, fetch, http_request) and a leading-curl Bash command.
//  3. LLM validator fallback for shapes the parser can't classify — returns
//     a structured verdict with `ambiguous=true` for inputs it can't decide.
//  4. Boundary check — validator's `target_host` must match the placeholder's
//     bound service host allowlist; mismatch => fail-closed.
//
// The validator is THE authorization boundary. Restriction policy and
// approval prompts run against the verdict; therefore validator-claimed
// fields must be bounded by ground truth (placeholder bound-service for
// host; harness-observed request for method/path) at downstream sites.
package inspector

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
)

// ToolUse is a parsed assistant tool_use block under inspection.
type ToolUse struct {
	// ID is the upstream-provided block ID (e.g. Anthropic's `toolu_…`).
	ID string
	// Name is the tool name as the harness declared it (`Bash`, `WebFetch`,
	// `fetch`, etc.).
	Name string
	// Input is the raw JSON of the tool call's arguments.
	Input json.RawMessage
}

// Verdict carries the inspector's decision about a tool_use.
type Verdict struct {
	// IsAPICall is true when this is a credentialed HTTP call we should
	// mediate. False indicates the substring trigger was a false positive
	// (placeholder appeared in a log line, comment, etc.) — pass through.
	IsAPICall bool

	// Ambiguous flips to true when the inspector can't be confident.
	// Triggers fail-closed at the rewriter.
	Ambiguous bool

	// Method is the inferred HTTP verb (uppercased: GET, POST, ...).
	Method string

	// Host is the inferred target host (e.g. `api.github.com`). The
	// boundary check downstream MUST validate this against the placeholder's
	// bound service host allowlist before the verdict authorizes anything.
	Host string

	// Path is the inferred URL path including query. Used for restriction
	// matching once the boundary check confirms host.
	Path string

	// CredentialLocations names where the placeholder appears in the
	// request. Each entry is something like {"header","Authorization","Bearer"}.
	CredentialLocations []CredentialLocation

	// Placeholders are the actual `autovault_…` strings the inspector
	// found in the tool_use input. Used by the boundary check to look
	// up the bound service for each placeholder; must be non-empty for
	// IsAPICall to authorize a rewrite.
	Placeholders []string

	// Source describes which arm of the pipeline produced this verdict —
	// useful for telemetry and audit.
	Source VerdictSource

	// Reason is a short human-readable note on why the verdict came out
	// this way. Audit-only, not surfaced to the model.
	Reason string
}

// CredentialLocation describes a single position where a placeholder is
// embedded in the request being prepared.
type CredentialLocation struct {
	// Kind is "header" for v1; "query" / "body" arrive in Phase 4.
	Kind string

	// Name is the header name, query parameter name, or body field path.
	Name string

	// Scheme is the auth scheme when applicable ("Bearer", "Basic", ...).
	Scheme string
}

// VerdictSource records the path that produced the verdict. Useful for
// telemetry; the rewriter and audit log both surface it.
type VerdictSource string

const (
	SourceTriggerMiss   VerdictSource = "trigger_miss"
	SourceDeterministic VerdictSource = "deterministic"
	SourceValidator     VerdictSource = "validator"
)

// Inspector orchestrates the deterministic parser + LLM validator fallback.
// Construct via NewInspector; the zero value is not usable.
type Inspector struct {
	Parser    Parser
	Validator Validator
}

// NewInspector returns a default Inspector. The Parser handles known
// structured tool shapes and clean curl invocations; the Validator falls
// back for anything else (and will be Haiku-backed in production).
func NewInspector(p Parser, v Validator) *Inspector {
	return &Inspector{Parser: p, Validator: v}
}

// Inspect runs the trigger fast-path, then the parser, then the validator.
// Always returns a verdict (never nil); errors from the validator are
// converted to ambiguous=true so the rewriter fails closed without leaking
// a low-signal exception to the caller.
func (i *Inspector) Inspect(ctx context.Context, t ToolUse) Verdict {
	if !TriggerHits(t) {
		return Verdict{
			IsAPICall: false,
			Source:    SourceTriggerMiss,
			Reason:    "no autovault placeholder substring",
		}
	}

	if i != nil && i.Parser != nil {
		if v, ok := i.Parser.Parse(t); ok {
			v.Source = SourceDeterministic
			return v
		}
	}

	if i == nil || i.Validator == nil {
		return Verdict{
			IsAPICall: false,
			Ambiguous: true,
			Source:    SourceValidator,
			Reason:    "no validator configured; failing closed on unparseable shape",
		}
	}

	v, err := i.Validator.Validate(ctx, t)
	if err != nil {
		return Verdict{
			IsAPICall: false,
			Ambiguous: true,
			Source:    SourceValidator,
			Reason:    "validator error: " + err.Error(),
		}
	}
	v.Source = SourceValidator
	// The validator's structured response intentionally doesn't carry
	// Placeholders — the LLM can't be trusted to enumerate them. Extract
	// from the raw input bytes here so the downstream BoundaryCheck has
	// something to validate against. Without this, validator-path verdicts
	// always fail the boundary check (Placeholders required to be non-empty).
	if len(v.Placeholders) == 0 {
		v.Placeholders = extractPlaceholdersFromInput(t.Input)
	}
	return v
}

// extractPlaceholdersFromInput finds every `autovault_…` substring in
// the raw tool_use input bytes. Used as a fallback when the verdict
// source doesn't populate Placeholders directly (i.e. the LLM
// validator path).
func extractPlaceholdersFromInput(input []byte) []string {
	if len(input) == 0 {
		return nil
	}
	matches := shadowPlaceholderExtractRE.FindAllSubmatch(input, -1)
	if len(matches) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(matches))
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		if len(m) < 2 {
			continue
		}
		s := string(m[1])
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

// shadowPlaceholderExtractRE captures the placeholder token itself in
// capture group 1, anchoring on a non-alnum left boundary so embedded
// substrings (e.g. `myautovault_x`) do not produce phantom placeholder
// hits. The token-detector counterpart below uses the same boundary.
var shadowPlaceholderExtractRE = regexp.MustCompile(`(?i)(?:^|[^a-z0-9])(autovault[_:][a-z0-9._:-]+)`)

// TriggerHits reports whether a tool_use's serialized input contains
// an autovault placeholder token. Cheap pre-filter; the bulk of
// tool_uses skip credential inspection entirely.
func TriggerHits(t ToolUse) bool {
	if len(t.Input) == 0 {
		return false
	}
	return shadowPlaceholderTokenRE.Match(t.Input)
}

var shadowPlaceholderTokenRE = regexp.MustCompile(`(?i)(^|[^a-z0-9])autovault[_:][a-z0-9._:-]+`)

// Parser is the deterministic structural-shape parser.
type Parser interface {
	// Parse inspects a triggered tool_use and returns (verdict, true) when
	// it can confidently classify it. Returns (zero, false) for shapes it
	// doesn't recognize so the inspector can fall through to the validator.
	Parse(t ToolUse) (Verdict, bool)
}

// Validator is the LLM fallback. Implementations call out to a small model
// (Haiku in production) with a content-addressed prompt. Implementations
// MUST tolerate cancellation via ctx and SHOULD return ambiguous=true on
// inputs they can't decide rather than guessing.
type Validator interface {
	Validate(ctx context.Context, t ToolUse) (Verdict, error)
}

// AmbiguousValidator is a Validator that always returns ambiguous=true. It's
// the safe default in environments without a configured LLM (tests, pure
// passthrough, fail-closed degraded mode).
type AmbiguousValidator struct{}

// Validate implements Validator.
func (AmbiguousValidator) Validate(_ context.Context, _ ToolUse) (Verdict, error) {
	return Verdict{
		IsAPICall: false,
		Ambiguous: true,
		Reason:    "ambiguous validator (no LLM configured)",
	}, nil
}

// ErrNoMatch is returned by Parser implementations that need to signal a
// principled decline without using the (Verdict, bool) return shape — used
// internally by the deterministic parser when it deliberately recognizes a
// shape but refuses to rewrite it (e.g. unsupported Bash construct).
var ErrNoMatch = errors.New("inspector: no parser match")

// canonicalMethod returns method uppercased, defaulting to "GET" if empty.
// Use ONLY for inputs where the method is structurally known (the
// deterministic parser knows from the tool shape). For inputs sourced
// from the LLM validator — which may omit method — use canonicalMethodOrEmpty
// and mark the verdict ambiguous when method is missing.
func canonicalMethod(m string) string {
	m = strings.TrimSpace(strings.ToUpper(m))
	if m == "" {
		return "GET"
	}
	return m
}

// canonicalMethodOrEmpty uppercases and trims without defaulting. The
// caller decides what to do with an empty method (typically: mark the
// verdict ambiguous so authorization fails closed rather than acting
// on a phantom GET).
func canonicalMethodOrEmpty(m string) string {
	return strings.TrimSpace(strings.ToUpper(m))
}
