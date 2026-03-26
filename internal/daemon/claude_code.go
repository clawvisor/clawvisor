package daemon

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/clawvisor/clawvisor/skills"
)

// claudeCodeSetupCommand is the markdown template for the Claude Code
// /clawvisor-setup slash command. It is written to ~/.claude/commands/
// during daemon install when Claude Code is detected. The placeholders
// {{CLAWVISOR_BINARY}} and {{SKILL_PATH}} are replaced at install time.
const claudeCodeSetupCommand = `Set up Clawvisor in the current project so Claude Code can make gated API
requests (Gmail, Calendar, Drive, GitHub, Slack, etc.) through the Clawvisor
gateway with task-scoped authorization and human approval.

## Steps

### 1. Verify the daemon is running

` + "```bash" + `
curl -sf http://localhost:25297/ready 2>/dev/null && echo "RUNNING" || echo "NOT RUNNING"
` + "```" + `

If NOT RUNNING, tell the user to start it with ` + "`clawvisor start`" + ` and wait
for them to confirm before continuing.

### 2. Create an agent token

` + "```bash" + `
{{CLAWVISOR_BINARY}} agent create claude-code --replace --json
` + "```" + `

Parse the JSON output and save the ` + "`token`" + ` value — you will need it below.
If this fails, the daemon may not be running or the binary may not be on PATH.

### 3. Install the Clawvisor skill

Copy the pre-installed skill file into this project:

` + "```bash" + `
mkdir -p .claude/skills/clawvisor
cp {{SKILL_PATH}} .claude/skills/clawvisor/SKILL.md
` + "```" + `

### 4. Set environment variables

Write the agent token and daemon URL to ` + "`.claude/.env`" + `:

` + "```bash" + `
# Remove any previous Clawvisor lines
grep -v '^CLAWVISOR_' .claude/.env > /tmp/claude-env.tmp 2>/dev/null || true
mv /tmp/claude-env.tmp .claude/.env 2>/dev/null || true

# Append new values
cat >> .claude/.env <<EOF
CLAWVISOR_URL=http://localhost:25297
CLAWVISOR_AGENT_TOKEN=<token from step 2>
EOF
` + "```" + `

Then ensure ` + "`.claude/.env`" + ` is in ` + "`.gitignore`" + `:

` + "```bash" + `
grep -q '\\.claude/\\.env' .gitignore 2>/dev/null || echo '.claude/.env' >> .gitignore
` + "```" + `

### 5. Verify

` + "```bash" + `
curl -sf -H "Authorization: Bearer $CLAWVISOR_AGENT_TOKEN" \
  http://localhost:25297/api/skill/catalog | head -20
` + "```" + `

This should return a JSON service catalog. If it returns 401, the token is
wrong. If it fails to connect, the daemon is not running.

### 6. Done

Tell the user setup is complete. The Clawvisor skill will be loaded
automatically when relevant, or they can invoke it explicitly. Remind them to:

- Connect services in the Clawvisor dashboard (Services tab) before asking
  you to use them
- Approve tasks in the dashboard or via mobile when you request them
`

// installClaudeCodeCommand writes the /clawvisor-setup slash command to
// ~/.claude/commands/clawvisor-setup.md and a stripped copy of SKILL.md to
// ~/.clawvisor/SKILL.md. It resolves the current binary path and bakes both
// paths into the command template.
func installClaudeCodeCommand(dataDir string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolving home directory: %w", err)
	}

	// Write the stripped SKILL.md into the daemon data directory so the
	// slash command can copy it into projects without curling the daemon.
	skillDest := filepath.Join(dataDir, "SKILL.md")
	if err := writeStrippedSkill(skillDest); err != nil {
		return fmt.Errorf("writing skill file: %w", err)
	}

	commandsDir := filepath.Join(home, ".claude", "commands")
	if err := os.MkdirAll(commandsDir, 0755); err != nil {
		return fmt.Errorf("creating commands directory: %w", err)
	}

	// Resolve the clawvisor binary path.
	binary, err := os.Executable()
	if err != nil {
		binary = "clawvisor" // fallback to PATH lookup
	} else {
		resolved, err := filepath.EvalSymlinks(binary)
		if err == nil {
			binary = resolved
		}
		// If this is a go-run temp binary, fall back to bare name.
		if isGoRunBinary(binary) {
			binary = "clawvisor"
		}
	}

	content := claudeCodeSetupCommand
	content = strings.ReplaceAll(content, "{{CLAWVISOR_BINARY}}", binary)
	content = strings.ReplaceAll(content, "{{SKILL_PATH}}", skillDest)

	dest := filepath.Join(commandsDir, "clawvisor-setup.md")
	if err := os.WriteFile(dest, []byte(content), 0644); err != nil {
		return fmt.Errorf("writing command file: %w", err)
	}

	return nil
}

// writeStrippedSkill reads the embedded SKILL.md, strips the YAML frontmatter,
// and writes the result to dest.
func writeStrippedSkill(dest string) error {
	raw, err := skills.FS.ReadFile("clawvisor/SKILL.md")
	if err != nil {
		return fmt.Errorf("reading embedded SKILL.md: %w", err)
	}

	// Strip YAML frontmatter (the --- delimited block at the top).
	var b strings.Builder
	scanner := bufio.NewScanner(strings.NewReader(string(raw)))
	fences := 0
	for scanner.Scan() {
		line := scanner.Text()
		if line == "---" {
			fences++
			continue
		}
		if fences >= 2 {
			b.WriteString(line)
			b.WriteByte('\n')
		}
	}

	return os.WriteFile(dest, []byte(b.String()), 0644)
}

// hasClaudeCode reports whether the "claude" binary is in the detected agents list.
func hasClaudeCode(agents []knownAgent) bool {
	for _, a := range agents {
		if a.Binary == "claude" {
			return true
		}
	}
	return false
}

// hasClaudeDesktop reports whether Claude Desktop is in the detected agents list.
func hasClaudeDesktop(agents []knownAgent) bool {
	for _, a := range agents {
		if a.Binary == "claude-desktop" {
			return true
		}
	}
	return false
}

// offerClaudeDesktopSetup prints instructions for installing the Clawvisor
// cowork plugin in Claude Desktop.
func offerClaudeDesktopSetup() {
	fmt.Println()
	fmt.Println(bold.Padding(0, 2).Render("Claude Desktop"))
	fmt.Println(dim.Padding(0, 2).Render("  To connect Claude Desktop to Clawvisor, install the cowork plugin:"))
	fmt.Println()
	fmt.Println(dim.Padding(0, 2).Render("  1. Download the plugin:"))
	fmt.Println(green.Padding(0, 2).Render("     https://github.com/clawvisor/cowork-plugin"))
	fmt.Println()
	fmt.Println(dim.Padding(0, 2).Render("  2. In Claude Desktop: Settings → Plugins → Install from local source"))
	fmt.Println()
	fmt.Println(dim.Padding(0, 2).Render("  3. Restart Claude Desktop — it will prompt you to authorize via OAuth"))
	fmt.Println()
	fmt.Println(dim.Padding(0, 2).Render("  Full guide: https://github.com/clawvisor/clawvisor/blob/main/docs/INTEGRATE_CLAUDE_COWORK.md"))
	fmt.Println()
}

// offerClaudeCodeSetup prompts the user to install the /clawvisor-setup
// slash command for Claude Code.
func offerClaudeCodeSetup(dataDir string) error {
	install := true
	if err := huh.NewForm(
		huh.NewGroup(
			huh.NewConfirm().
				Title("Install the /clawvisor-setup command for Claude Code?").
				Description("Adds a slash command so you can run /clawvisor-setup\nin any project to connect Claude Code to this daemon.").
				Affirmative("Yes").
				Negative("No").
				Value(&install),
		),
	).Run(); err != nil {
		return err
	}
	if !install {
		return nil
	}

	if err := installClaudeCodeCommand(dataDir); err != nil {
		return err
	}

	fmt.Println(green.Padding(0, 2).Render("  ✓ Installed /clawvisor-setup command"))
	fmt.Println(dim.Padding(0, 2).Render("    Run /clawvisor-setup in Claude Code to connect a project."))
	fmt.Println()
	return nil
}
