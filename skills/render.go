package skills

import (
	"bytes"
	"fmt"
	"strings"
	"text/template"

	"github.com/clawvisor/clawvisor/pkg/version"
)

// Target identifies which rendering variant to produce.
type Target string

const (
	// TargetClaudeCode renders for Claude Code — curl-based examples, setup
	// instructions, full detail.
	TargetClaudeCode Target = "claude-code"

	// TargetCodex renders for Codex — curl-based examples like Claude Code,
	// since Codex executes shell and has no Clawvisor MCP server in this flow.
	// Distinct from TargetClaudeCode because Codex requires name+description
	// frontmatter and must not receive Claude Code's permission-glob rationale
	// for single-line curls, which does not apply to it.
	TargetCodex Target = "codex"

	// TargetCowork renders for the Claude Desktop (Cowork) plugin — MCP tool
	// names, no curl examples, no setup.
	TargetCowork Target = "cowork"

	// TargetMCP renders a condensed version for the MCP server's initialize
	// instructions — MCP tool names, minimal detail.
	TargetMCP Target = "mcp"
)

// RenderOptions holds optional overrides for template rendering.
type RenderOptions struct {
	// ClawvisorURL is the base URL for the Clawvisor instance. When set, the
	// template uses it as the concrete URL in setup instructions instead of a
	// generic placeholder. Empty means "not known at render time".
	ClawvisorURL string

	// ViaRelay is true when the skill is being served through the cloud relay.
	// The template uses this to include E2E encryption guidance.
	ViaRelay bool

	// FeedbackEnabled is true when the agent feedback system (report_bug, submit_nps)
	// is active on this instance. When false, the feedback documentation is omitted.
	FeedbackEnabled bool
}

// templateData holds the flags that control conditional rendering.
type templateData struct {
	Target           Target
	UseCurl          bool
	Condensed        bool
	ClawvisorURL     string // concrete instance URL, empty if unknown
	ViaRelay         bool   // true when served through the relay
	FeedbackEnabled  bool   // true when agent feedback tools are active
	SkillVersion     string // current skill version
	SkillPublishedAt string // date the skill version was published
}

// dataForTarget returns the template data for the given target.
//
// An unrecognised target is an error rather than a zero-value fallback. The old
// default arm returned templateData{Target: t}, which renders a document with
// NO frontmatter (Codex rejects those at startup) describing MCP tool names to
// a harness that has no MCP tools — and it did so while returning nil, so the
// caller had no way to notice. Failing loudly is the only safe behaviour for a
// value that reaches an unauthenticated HTTP handler.
func dataForTarget(t Target) (templateData, error) {
	switch t {
	case TargetClaudeCode:
		return templateData{
			Target:    t,
			UseCurl:   true,
			Condensed: false,
		}, nil
	case TargetCodex:
		return templateData{
			Target:    t,
			UseCurl:   true,
			Condensed: false,
		}, nil
	case TargetCowork:
		return templateData{
			Target:    t,
			UseCurl:   false,
			Condensed: false,
		}, nil
	case TargetMCP:
		return templateData{
			Target:    t,
			UseCurl:   false,
			Condensed: true,
		}, nil
	default:
		return templateData{}, fmt.Errorf("unknown skill target %q (want one of %s)",
			t, strings.Join(TargetNames(), ", "))
	}
}

// TargetNames lists every renderable target. Used for error messages and for
// validating a caller-supplied target (e.g. an HTTP query param) before render.
func TargetNames() []string {
	return []string{
		string(TargetClaudeCode),
		string(TargetCodex),
		string(TargetCowork),
		string(TargetMCP),
	}
}

// ParseTarget validates s against the known targets. Callers that accept a
// target from outside the process (HTTP query params, CLI argv) should use this
// rather than converting with Target(s), which silently produces an invalid
// value.
func ParseTarget(s string) (Target, error) {
	for _, name := range TargetNames() {
		if s == name {
			return Target(name), nil
		}
	}
	return "", fmt.Errorf("unknown skill target %q (want one of %s)",
		s, strings.Join(TargetNames(), ", "))
}

// Render produces the SKILL.md content for the given target by executing the
// embedded template with the appropriate flags.
func Render(target Target) (string, error) {
	return RenderWithOptions(target, RenderOptions{})
}

// RenderWithOptions is like Render but accepts additional options that
// customise the output (e.g. baking in a concrete CLAWVISOR_URL).
func RenderWithOptions(target Target, opts RenderOptions) (string, error) {
	raw, err := FS.ReadFile("clawvisor/SKILL.md.tmpl")
	if err != nil {
		return "", fmt.Errorf("reading SKILL.md.tmpl: %w", err)
	}

	tmpl, err := template.New("skill").Parse(string(raw))
	if err != nil {
		return "", fmt.Errorf("parsing SKILL.md.tmpl: %w", err)
	}

	data, err := dataForTarget(target)
	if err != nil {
		return "", err
	}
	data.ClawvisorURL = opts.ClawvisorURL
	data.ViaRelay = opts.ViaRelay
	data.FeedbackEnabled = opts.FeedbackEnabled
	data.SkillVersion = version.Version
	data.SkillPublishedAt = version.SkillPublishedAt

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("executing SKILL.md.tmpl: %w", err)
	}

	return strings.TrimLeft(buf.String(), "\n"), nil
}
