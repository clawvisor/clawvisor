package skills

import (
	"bytes"
	"fmt"
	"strings"
	"text/template"
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

// templateData holds the flags that control conditional rendering.
type templateData struct {
	Target    Target
	UseCurl   bool
	Condensed bool
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
	raw, err := FS.ReadFile("clawvisor/SKILL.md.tmpl")
	if err != nil {
		return "", fmt.Errorf("reading SKILL.md.tmpl: %w", err)
	}

	tmpl, err := template.New("skill").Parse(string(raw))
	if err != nil {
		return "", fmt.Errorf("parsing SKILL.md.tmpl: %w", err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, dataForTarget(target)); err != nil {
		return "", fmt.Errorf("executing SKILL.md.tmpl: %w", err)
	}

	return strings.TrimLeft(buf.String(), "\n"), nil
}
