package daemon

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/clawvisor/clawvisor/pkg/version"
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

After copying, read ` + "`.claude/skills/clawvisor/SKILL.md`" + ` so you understand how
to use the Clawvisor skill in the smoke test later.

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

### 6. Auto-approve Clawvisor curl requests (optional)

First, check whether auto-approve rules already exist. Read
` + "`~/.claude/settings.json`" + ` and look for entries matching
` + "`Bash(curl *http://localhost:25297/*)`" + ` in ` + "`permissions.allow`" + `.

If the rules are already present, skip this step silently.

If not, ask the user if they'd like to auto-approve curl requests to the
Clawvisor daemon so Claude Code won't prompt for each one. If they agree,
ensure both ` + "`~/.claude/settings.json`" + ` (user-level) and
` + "`.claude/settings.json`" + ` (project-level) have the following entries in
` + "`permissions.allow`" + `:

` + "```json" + `
{
  "permissions": {
    "allow": [
      "Bash(curl *http://localhost:25297/*)",
      "Bash(curl *{{RELAY_ORIGIN}}/*)"
    ]
  }
}
` + "```" + `

Merge with any existing settings — don't overwrite other entries. If an entry
already exists, skip it.

### 7. End-to-end smoke test

Now that everything is configured, run a quick smoke test to prove the full
flow works. Use the Clawvisor skill to:

1. **Create a test task** — pick any connected service visible in the catalog
   (e.g. Gmail, Calendar, GitHub) and create a task with a narrow scope such as
   "read my most recent email subject" or "list my GitHub notifications".
   Tell the user to approve the task in the Clawvisor dashboard or mobile app,
   then wait for approval before continuing.

2. **Make an in-scope request** — once approved, make a gateway call that falls
   within the task's approved scope. Show the user the successful response.

3. **Make an out-of-scope request** — make a second gateway call using the same
   task that is clearly outside the approved scope (e.g. sending an email when
   the task only allows reading). Show the user that this request is rejected,
   demonstrating that Clawvisor enforces task boundaries.

Summarize the results: the in-scope call should have succeeded and the
out-of-scope call should have been denied. If either result is unexpected,
help the user debug.

### 8. Done

Tell the user setup is complete. The Clawvisor skill will be loaded
automatically when relevant, or they can invoke it explicitly. Remind them to:

- Connect services in the Clawvisor dashboard (Services tab) before asking
  you to use them
- Approve tasks in the dashboard or via mobile when you request them

### 9. Offer to uninstall /clawvisor-setup (optional)

Now that setup is complete, ask the user if they'd like to remove the
` + "`/clawvisor-setup`" + ` slash command since it's no longer needed. If they agree:

` + "```bash" + `
rm ~/.claude/commands/clawvisor-setup.md
` + "```" + `

If they decline, remind them they can delete it later with the same command.
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

	relayOrigin := "https://relay.clawvisor.com"
	if version.IsStaging() {
		relayOrigin = "https://relay.staging.clawvisor.com"
	}

	content := claudeCodeSetupCommand
	content = strings.ReplaceAll(content, "{{CLAWVISOR_BINARY}}", binary)
	content = strings.ReplaceAll(content, "{{SKILL_PATH}}", skillDest)
	content = strings.ReplaceAll(content, "{{RELAY_ORIGIN}}", relayOrigin)

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

	// Append Claude Code-specific guidance about curl formatting.
	b.WriteString("\n## Important: Single-line curl commands\n\n")
	b.WriteString("Always write curl commands as a **single line** — do not use `\\` line\n")
	b.WriteString("continuations. Claude Code's permission auto-approve rules use glob patterns\n")
	b.WriteString("where `*` does not match newline characters, so multi-line commands will\n")
	b.WriteString("trigger a manual approval prompt even when a matching rule exists.\n")

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

	// Offer to auto-approve Clawvisor curl requests globally.
	if err := offerClaudeCodeCurlPermission(); err != nil {
		return err
	}

	return nil
}

// offerClaudeCodeCurlPermission prompts the user to add auto-approval rules
// for Clawvisor curl requests to ~/.claude/settings.json so Claude Code won't
// prompt for each one.
func offerClaudeCodeCurlPermission() error {
	allowCurl := true
	if err := huh.NewForm(
		huh.NewGroup(
			huh.NewConfirm().
				Title("Auto-approve curl requests to the Clawvisor daemon?").
				Description("Adds permission rules to ~/.claude/settings.json so\nClaude Code won't prompt for each Clawvisor API call.").
				Affirmative("Yes").
				Negative("No").
				Value(&allowCurl),
		),
	).Run(); err != nil {
		return err
	}
	if !allowCurl {
		return nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolving home directory: %w", err)
	}

	settingsPath := filepath.Join(home, ".claude", "settings.json")

	relayOrigin := "https://relay.clawvisor.com"
	if version.IsStaging() {
		relayOrigin = "https://relay.staging.clawvisor.com"
	}

	rules := []string{
		"Bash(curl *http://localhost:25297/*)",
		fmt.Sprintf("Bash(curl *%s/*)", relayOrigin),
	}

	if err := addClaudePermissionRules(settingsPath, rules); err != nil {
		return fmt.Errorf("updating Claude Code settings: %w", err)
	}

	fmt.Println(green.Padding(0, 2).Render("  ✓ Auto-approve rules added to ~/.claude/settings.json"))
	fmt.Println()
	return nil
}

// addClaudePermissionRules reads a Claude Code settings.json file, adds the
// given rules to permissions.allow (deduplicating), and writes it back.
func addClaudePermissionRules(path string, rules []string) error {
	// Read existing settings or start fresh.
	var settings map[string]any
	data, err := os.ReadFile(path)
	if err == nil {
		if err := json.Unmarshal(data, &settings); err != nil {
			return fmt.Errorf("parsing %s: %w", path, err)
		}
	} else if os.IsNotExist(err) {
		settings = make(map[string]any)
	} else {
		return fmt.Errorf("reading %s: %w", path, err)
	}

	// Navigate to permissions.allow, creating intermediate objects as needed.
	perms, ok := settings["permissions"].(map[string]any)
	if !ok {
		perms = make(map[string]any)
		settings["permissions"] = perms
	}

	var allow []any
	if existing, ok := perms["allow"].([]any); ok {
		allow = existing
	}

	// Build a set of existing entries for dedup.
	existing := make(map[string]bool, len(allow))
	for _, v := range allow {
		if s, ok := v.(string); ok {
			existing[s] = true
		}
	}

	for _, rule := range rules {
		if !existing[rule] {
			allow = append(allow, rule)
		}
	}
	perms["allow"] = allow

	// Write back with indentation.
	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return fmt.Errorf("marshalling settings: %w", err)
	}

	// Ensure parent directory exists.
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}

	return os.WriteFile(path, append(out, '\n'), 0644)
}
