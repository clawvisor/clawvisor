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
}

// templateData holds the flags that control conditional rendering.
type templateData struct {
	Target           Target
	UseCurl          bool
	Condensed        bool
	ClawvisorURL     string // concrete instance URL, empty if unknown
	ViaRelay         bool   // true when served through the relay
	SkillVersion     string // current skill version
	SkillPublishedAt string // date the skill version was published
}

// dataForTarget returns the template data for the given target.
func dataForTarget(t Target) templateData {
	switch t {
	case TargetClaudeCode:
		return templateData{
			Target:    t,
			UseCurl:   true,
			Condensed: false,
		}
	case TargetCowork:
		return templateData{
			Target:    t,
			UseCurl:   false,
			Condensed: false,
		}
	case TargetMCP:
		return templateData{
			Target:    t,
			UseCurl:   false,
			Condensed: true,
		}
	default:
		return templateData{Target: t}
	}
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

	data := dataForTarget(target)
	data.ClawvisorURL = opts.ClawvisorURL
	data.ViaRelay = opts.ViaRelay
	data.SkillVersion = version.Version
	data.SkillPublishedAt = version.SkillPublishedAt

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("executing SKILL.md.tmpl: %w", err)
	}

	return strings.TrimLeft(buf.String(), "\n"), nil
}
