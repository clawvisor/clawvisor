package daemon

import (
	"crypto/md5" //nolint:gosec // used only for cache key derivation matching mcp-remote's convention
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/charmbracelet/huh"
	"github.com/clawvisor/clawvisor/coworkplugin"
	"github.com/clawvisor/clawvisor/pkg/version"
	"github.com/clawvisor/clawvisor/skills"
)

// claudeCodeSetupCommand is the markdown content for the Claude Code
// /clawvisor-setup slash command. It is written to ~/.claude/commands/
// during daemon install when Claude Code is detected.
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

### 2. Connect as an agent

Register with the daemon and wait for the user to approve:

` + "```bash" + `
curl -s -X POST "http://localhost:25297/api/agents/connect?wait=true&timeout=120" -H "Content-Type: application/json" -d '{"name": "claude-code", "description": "Claude Code agent"}'
` + "```" + `

This sends a connection request to the daemon. The user will be notified
to approve. The ` + "`?wait=true`" + ` parameter makes the request block until the
user approves (or the timeout elapses).

Parse the JSON response. If ` + "`status`" + ` is ` + "`approved`" + `, save the ` + "`token`" + `
value — you will need it below. If the timeout elapses and ` + "`status`" + ` is
still ` + "`pending`" + `, tell the user to approve the connection request in the
Clawvisor dashboard and long-poll ` + "`GET /api/agents/connect/{connection_id}/status`" + `
until it resolves.

### 3. Set environment variables

Save the agent token and daemon URL to ` + "`~/.claude/settings.json`" + ` so they
persist across all Claude Code sessions and projects.

Read ` + "`~/.claude/settings.json`" + ` (create it if it doesn't exist). Merge the
following into the ` + "`env`" + ` object at the top level, preserving all other keys:

` + "```json" + `
{
  "env": {
    "CLAWVISOR_URL": "http://localhost:25297",
    "CLAWVISOR_AGENT_TOKEN": "<token from step 2>"
  }
}
` + "```" + `

Write the updated JSON back to ` + "`~/.claude/settings.json`" + `. The variables
will be available in every future Claude Code session without any per-project
setup.

### 4. Verify

` + "```bash" + `
curl -sf -H "Authorization: Bearer $CLAWVISOR_AGENT_TOKEN" \
  http://localhost:25297/api/skill/catalog | head -20
` + "```" + `

This should return a JSON service catalog. If it returns 401, the token is
wrong. If it fails to connect, the daemon is not running.

### 5. End-to-end smoke test

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

### 6. Done

Tell the user setup is complete. The Clawvisor skill will be loaded
automatically when relevant, or they can invoke it explicitly. Remind them to:

- Connect services in the Clawvisor dashboard (Services tab) before asking
  you to use them
- Approve tasks in the dashboard or via mobile when you request them

### 7. Offer to uninstall /clawvisor-setup (optional)

Now that setup is complete, ask the user if they'd like to remove the
` + "`/clawvisor-setup`" + ` slash command since it's no longer needed. If they agree:

` + "```bash" + `
rm ~/.claude/commands/clawvisor-setup.md
` + "```" + `

If they decline, remind them they can delete it later with the same command.
`

// installClaudeCodeSkill writes the Clawvisor skill globally to
// ~/.claude/skills/clawvisor/SKILL.md so it's available in all projects.
func installClaudeCodeSkill() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolving home directory: %w", err)
	}

	skillDir := filepath.Join(home, ".claude", "skills", "clawvisor")
	if err := os.MkdirAll(skillDir, 0755); err != nil {
		return fmt.Errorf("creating skill directory: %w", err)
	}
	return writeSkillWithCurlGuidance(filepath.Join(skillDir, "SKILL.md"))
}

// installClaudeCodeSetupCommand writes the /clawvisor-setup slash command to
// ~/.claude/commands/clawvisor-setup.md.
func installClaudeCodeSetupCommand() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolving home directory: %w", err)
	}

	commandsDir := filepath.Join(home, ".claude", "commands")
	if err := os.MkdirAll(commandsDir, 0755); err != nil {
		return fmt.Errorf("creating commands directory: %w", err)
	}

	dest := filepath.Join(commandsDir, "clawvisor-setup.md")
	if err := os.WriteFile(dest, []byte(claudeCodeSetupCommand), 0644); err != nil {
		return fmt.Errorf("writing command file: %w", err)
	}

	return nil
}

// writeSkillWithCurlGuidance renders the SKILL.md template for Claude Code,
// preserves the YAML frontmatter (required for Claude Code to recognize the
// skill), appends Claude Code-specific guidance, and writes the result to dest.
func writeSkillWithCurlGuidance(dest string) error {
	rendered, err := skills.Render(skills.TargetClaudeCode)
	if err != nil {
		return fmt.Errorf("rendering SKILL.md: %w", err)
	}

	var b strings.Builder
	b.WriteString(rendered)

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

// claudeDesktopConfigPath returns the platform-specific path to
// claude_desktop_config.json, or "" if unsupported.
func claudeDesktopConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "Claude", "claude_desktop_config.json")
	case "linux":
		// XDG default for Claude Desktop on Linux.
		if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
			return filepath.Join(xdg, "Claude", "claude_desktop_config.json")
		}
		return filepath.Join(home, ".config", "Claude", "claude_desktop_config.json")
	default:
		return ""
	}
}

// installClaudeDesktopMCPConfig adds the clawvisor-local MCP server entry to
// Claude Desktop's config file, merging with any existing configuration.
func installClaudeDesktopMCPConfig(configPath string) error {
	// Read existing config or start with an empty object.
	existing := make(map[string]json.RawMessage)
	if data, err := os.ReadFile(configPath); err == nil {
		if err := json.Unmarshal(data, &existing); err != nil {
			return fmt.Errorf("parsing existing config: %w", err)
		}
	}

	// Parse existing mcpServers or start fresh.
	servers := make(map[string]json.RawMessage)
	if raw, ok := existing["mcpServers"]; ok {
		if err := json.Unmarshal(raw, &servers); err != nil {
			return fmt.Errorf("parsing mcpServers: %w", err)
		}
	}

	// Add/overwrite the clawvisor-local entry.
	entry := map[string]any{
		"command": "npx",
		"args":    []string{"mcp-remote", "http://localhost:25297/mcp"},
	}
	entryJSON, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("marshalling MCP entry: %w", err)
	}
	servers["clawvisor-local"] = json.RawMessage(entryJSON)

	// Write servers back into the top-level config.
	serversJSON, err := json.Marshal(servers)
	if err != nil {
		return fmt.Errorf("marshalling mcpServers: %w", err)
	}
	existing["mcpServers"] = json.RawMessage(serversJSON)

	// Ensure the parent directory exists.
	if err := os.MkdirAll(filepath.Dir(configPath), 0755); err != nil {
		return fmt.Errorf("creating config directory: %w", err)
	}

	out, err := json.MarshalIndent(existing, "", "  ")
	if err != nil {
		return fmt.Errorf("marshalling config: %w", err)
	}
	return os.WriteFile(configPath, append(out, '\n'), 0644)
}

// clearMCPRemoteCache removes mcp-remote's cached OAuth state for the local
// daemon. mcp-remote caches client_info, tokens, and lock files under
// ~/.mcp-auth/<version>/<md5(server_url)>_*.json. If the daemon's database
// was recreated, the cached client_id becomes stale and causes "unknown
// client_id" errors during OAuth authorization. Clearing forces mcp-remote
// to re-register via /oauth/register on next startup.
func clearMCPRemoteCache() {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}

	authDir := filepath.Join(home, ".mcp-auth")
	entries, err := os.ReadDir(authDir)
	if err != nil {
		return
	}

	// MD5 of the MCP endpoint URL, matching mcp-remote's key derivation.
	h := md5.Sum([]byte("http://localhost:25297/mcp")) //nolint:gosec
	prefix := hex.EncodeToString(h[:]) + "_"

	for _, versionDir := range entries {
		if !versionDir.IsDir() {
			continue
		}
		dir := filepath.Join(authDir, versionDir.Name())
		files, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, f := range files {
			if strings.HasPrefix(f.Name(), prefix) {
				_ = os.Remove(filepath.Join(dir, f.Name()))
			}
		}
	}
}

// restartClaudeDesktop kills and relaunches Claude Desktop so it picks up
// the new MCP config.
func restartClaudeDesktop() error {
	switch runtime.GOOS {
	case "darwin":
		// Quit gracefully via AppleScript, then reopen.
		_ = exec.Command("osascript", "-e", `tell application "Claude" to quit`).Run()
		// Brief pause to let the process exit.
		exec.Command("sleep", "1").Run()
		return exec.Command("open", "-a", "Claude").Start()
	case "linux":
		_ = exec.Command("pkill", "-f", "claude-desktop").Run()
		exec.Command("sleep", "1").Run()
		return exec.Command("claude-desktop").Start()
	default:
		return fmt.Errorf("unsupported platform")
	}
}

// offerClaudeDesktopSetup interactively configures the MCP connection for
// Claude Desktop. It writes the MCP server entry to claude_desktop_config.json
// and optionally restarts Claude Desktop. The user only needs to click
// "Authorize" on the OAuth prompt that appears after restart.
func offerClaudeDesktopSetup() {
	if nonInteractive() {
		configPath := claudeDesktopConfigPath()
		if configPath == "" {
			return
		}
		if err := installClaudeDesktopMCPConfig(configPath); err != nil {
			fmt.Println(dim.Padding(0, 2).Render("  Warning: could not configure Claude Desktop: " + err.Error()))
			return
		}
		fmt.Println(green.Padding(0, 2).Render("  ✓ Configured Claude Desktop MCP"))
		if err := installCoworkPlugin(); err != nil {
			fmt.Println(dim.Padding(0, 2).Render("  Warning: could not install Cowork plugin: " + err.Error()))
		} else {
			fmt.Println(green.Padding(0, 2).Render("  ✓ Installed Clawvisor Cowork plugin"))
		}
		fmt.Println(dim.Padding(0, 2).Render("  Restart Claude Desktop and authorize when prompted."))
		return
	}

	configPath := claudeDesktopConfigPath()
	if configPath == "" {
		// Unsupported platform — fall back to manual instructions.
		printClaudeDesktopManualInstructions()
		return
	}

	fmt.Println()
	fmt.Println(bold.Padding(0, 2).Render("Claude Desktop"))

	configure := true
	if err := huh.NewForm(
		huh.NewGroup(
			huh.NewConfirm().
				Title("Configure Claude Desktop to connect via MCP?").
				Description("Adds the Clawvisor MCP server to Claude Desktop's config.\nAfter restart, Claude Desktop will prompt you to authorize via OAuth.").
				Affirmative("Yes").
				Negative("No").
				Value(&configure),
		),
	).Run(); err != nil || !configure {
		if !configure {
			printClaudeDesktopManualInstructions()
		}
		return
	}

	if err := installClaudeDesktopMCPConfig(configPath); err != nil {
		fmt.Println(yellow.Padding(0, 2).Render("  Could not write config: " + err.Error()))
		fmt.Println()
		printClaudeDesktopManualInstructions()
		return
	}

	fmt.Println(green.Padding(0, 2).Render("  ✓ MCP server added to Claude Desktop config"))
	fmt.Println(dim.Padding(0, 2).Render("  " + configPath))

	// Install the Cowork plugin (commands, skills, CLAUDE.md).
	if err := installCoworkPlugin(); err != nil {
		fmt.Println(yellow.Padding(0, 2).Render("  Could not install Cowork plugin: " + err.Error()))
		fmt.Println(dim.Padding(0, 2).Render("  You can install it manually: Settings → Plugins → Install from local source"))
	} else {
		fmt.Println(green.Padding(0, 2).Render("  ✓ Installed Clawvisor Cowork plugin"))
	}
	fmt.Println()

	// Offer to restart Claude Desktop.
	restart := true
	if err := huh.NewForm(
		huh.NewGroup(
			huh.NewConfirm().
				Title("Restart Claude Desktop now?").
				Description("Claude Desktop needs to restart to pick up the new config.\nIt will prompt you to authorize Clawvisor via OAuth.").
				Affirmative("Yes").
				Negative("I'll restart later").
				Value(&restart),
		),
	).Run(); err != nil || !restart {
		fmt.Println(dim.Padding(0, 2).Render("  Restart Claude Desktop when you're ready — it will prompt you to authorize."))
		fmt.Println()
		return
	}

	// Clear stale mcp-remote OAuth cache so it re-registers with the
	// current daemon instead of reusing a client_id from a previous DB.
	clearMCPRemoteCache()

	if err := restartClaudeDesktop(); err != nil {
		fmt.Println(yellow.Padding(0, 2).Render("  Could not restart: " + err.Error()))
		fmt.Println(dim.Padding(0, 2).Render("  Restart Claude Desktop manually — it will prompt you to authorize."))
	} else {
		fmt.Println(green.Padding(0, 2).Render("  ✓ Claude Desktop restarting"))
		fmt.Println(dim.Padding(0, 2).Render("  Authorize Clawvisor when the OAuth prompt appears."))
	}
	fmt.Println()
}

// printClaudeDesktopManualInstructions prints a short pointer to the
// dedicated integrate subcommand.
func printClaudeDesktopManualInstructions() {
	fmt.Println()
	fmt.Println(dim.Padding(0, 2).Render("  To set up Claude Desktop later, run:"))
	fmt.Println(green.Padding(0, 2).Render("    clawvisor integrate claude-desktop"))
	fmt.Println()
}

// offerClaudeCodeSetup walks the user through Claude Code integration:
// 1. Install the Clawvisor skill globally
// 2. Install the /clawvisor-setup slash command
// 3. Add auto-approve rules for curl requests
func offerClaudeCodeSetup(_ string) error {
	fmt.Println()
	fmt.Println(bold.Padding(0, 2).Render("Claude Code"))

	// 1. Offer to install the skill globally.
	installSkill := true
	if err := huh.NewForm(
		huh.NewGroup(
			huh.NewConfirm().
				Title("Install the Clawvisor skill for Claude Code?").
				Description("Writes the skill to ~/.claude/skills/clawvisor/SKILL.md\nso Claude Code can use Clawvisor in any project.").
				Affirmative("Yes").
				Negative("No").
				Value(&installSkill),
		),
	).Run(); err != nil {
		return err
	}
	if !installSkill {
		return nil
	}

	if err := installClaudeCodeSkill(); err != nil {
		return err
	}
	fmt.Println(green.Padding(0, 2).Render("  ✓ Installed Clawvisor skill to ~/.claude/skills/clawvisor/SKILL.md"))

	// 2. Offer to install the /clawvisor-setup slash command.
	installSetup := true
	if err := huh.NewForm(
		huh.NewGroup(
			huh.NewConfirm().
				Title("Install the /clawvisor-setup command?").
				Description("Adds a slash command so you can run /clawvisor-setup\nin Claude Code to create an agent token and connect.").
				Affirmative("Yes").
				Negative("No").
				Value(&installSetup),
		),
	).Run(); err != nil {
		return err
	}
	if installSetup {
		if err := installClaudeCodeSetupCommand(); err != nil {
			return err
		}
		fmt.Println(green.Padding(0, 2).Render("  ✓ Installed /clawvisor-setup command"))
		fmt.Println(dim.Padding(0, 2).Render("    Run /clawvisor-setup in Claude Code to connect."))
	}
	fmt.Println()

	// 3. Offer to auto-approve Clawvisor curl requests globally.
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
		"Bash(curl *$CLAWVISOR_URL/*)",
		fmt.Sprintf("Bash(curl *%s/*)", relayOrigin),
	}

	if err := addClaudePermissionRules(settingsPath, rules); err != nil {
		return fmt.Errorf("updating Claude Code settings: %w", err)
	}

	fmt.Println(green.Padding(0, 2).Render("  ✓ Auto-approve rules added to ~/.claude/settings.json"))
	fmt.Println()
	return nil
}

// installCoworkPlugin copies the embedded Clawvisor Cowork plugin into Claude
// Desktop's plugin directories. The plugin system uses three locations:
//
//  1. marketplaces/local-desktop-app-uploads/<name>/ — the marketplace source
//  2. cache/local-desktop-app-uploads/<name>/<version>/ — the installed copy
//  3. installed_plugins.json — the registry of installed plugins
//  4. .install-manifests/<name>@local-desktop-app-uploads.json — file checksums
//
// All four must be populated for Claude Desktop to recognize the plugin.
func installCoworkPlugin() error {
	pluginsDir, err := findCoworkPluginsDir()
	if err != nil {
		return err
	}

	// Read the plugin version from the embedded plugin.json.
	pluginJSON, err := coworkplugin.FS.ReadFile("plugin/.claude-plugin/plugin.json")
	if err != nil {
		return fmt.Errorf("reading embedded plugin.json: %w", err)
	}
	var meta struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	}
	if err := json.Unmarshal(pluginJSON, &meta); err != nil {
		return fmt.Errorf("parsing plugin.json: %w", err)
	}

	cacheDir := filepath.Join(pluginsDir, "cache", "local-desktop-app-uploads", meta.Name, meta.Version)
	marketplaceDir := filepath.Join(pluginsDir, "marketplaces", "local-desktop-app-uploads", meta.Name)

	// Render the SKILL.md from the shared template for the Cowork target.
	renderedSkill, err := skills.Render(skills.TargetCowork)
	if err != nil {
		return fmt.Errorf("rendering SKILL.md: %w", err)
	}

	// Copy embedded files to both the cache and marketplace directories.
	// The SKILL.md is replaced with the rendered version from the template.
	// Track file hashes for the install manifest.
	fileHashes := make(map[string]string)

	writeFile := func(rel string, data []byte) error {
		h := sha256.Sum256(data)
		fileHashes[rel] = hex.EncodeToString(h[:])

		for _, dir := range []string{cacheDir, marketplaceDir} {
			dest := filepath.Join(dir, rel)
			if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
				return err
			}
			if err := os.WriteFile(dest, data, 0644); err != nil {
				return err
			}
		}
		return nil
	}

	if err := fs.WalkDir(coworkplugin.FS, "plugin", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		rel, err := filepath.Rel("plugin", path)
		if err != nil {
			return err
		}

		if d.IsDir() {
			if err := os.MkdirAll(filepath.Join(cacheDir, rel), 0755); err != nil {
				return err
			}
			return os.MkdirAll(filepath.Join(marketplaceDir, rel), 0755)
		}

		// Replace the embedded SKILL.md with the template-rendered version.
		if rel == filepath.Join("skills", "clawvisor", "SKILL.md") {
			return writeFile(rel, []byte(renderedSkill))
		}

		data, err := coworkplugin.FS.ReadFile(path)
		if err != nil {
			return fmt.Errorf("reading embedded %s: %w", path, err)
		}

		return writeFile(rel, data)
	}); err != nil {
		return fmt.Errorf("copying plugin files: %w", err)
	}

	// Update installed_plugins.json registry.
	pluginID := meta.Name + "@local-desktop-app-uploads"
	now := nowUTC()
	if err := updateInstalledPlugins(pluginsDir, pluginID, cacheDir, meta.Version, now); err != nil {
		return fmt.Errorf("updating plugin registry: %w", err)
	}

	// Write install manifest.
	if err := writeInstallManifest(pluginsDir, pluginID, fileHashes, now); err != nil {
		return fmt.Errorf("writing install manifest: %w", err)
	}

	return nil
}

// nowUTC returns the current time formatted as an RFC3339 string with
// millisecond precision, matching the format used by Claude Desktop.
func nowUTC() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
}

// updateInstalledPlugins adds or updates the plugin entry in installed_plugins.json.
func updateInstalledPlugins(pluginsDir, pluginID, installPath, version, now string) error {
	path := filepath.Join(pluginsDir, "installed_plugins.json")

	var registry struct {
		Version int                        `json:"version"`
		Plugins map[string]json.RawMessage `json:"plugins"`
	}

	data, err := os.ReadFile(path)
	if err == nil {
		if err := json.Unmarshal(data, &registry); err != nil {
			return fmt.Errorf("parsing installed_plugins.json: %w", err)
		}
	} else if os.IsNotExist(err) {
		registry.Version = 2
		registry.Plugins = make(map[string]json.RawMessage)
	} else {
		return err
	}

	if registry.Plugins == nil {
		registry.Plugins = make(map[string]json.RawMessage)
	}

	entry := []map[string]any{
		{
			"scope":       "user",
			"installPath": installPath,
			"version":     version,
			"installedAt": now,
			"lastUpdated": now,
		},
	}

	entryJSON, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	registry.Plugins[pluginID] = json.RawMessage(entryJSON)

	out, err := json.MarshalIndent(registry, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(out, '\n'), 0644)
}

// writeInstallManifest creates the .install-manifests/<pluginID>.json file
// with SHA-256 checksums of all plugin files.
func writeInstallManifest(pluginsDir, pluginID string, fileHashes map[string]string, now string) error {
	manifestDir := filepath.Join(pluginsDir, ".install-manifests")
	if err := os.MkdirAll(manifestDir, 0755); err != nil {
		return err
	}

	manifest := map[string]any{
		"pluginId":  pluginID,
		"createdAt": now,
		"files":     fileHashes,
	}

	out, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}

	path := filepath.Join(manifestDir, pluginID+".json")
	return os.WriteFile(path, append(out, '\n'), 0644)
}

// findCoworkPluginsDir searches for the cowork_plugins directory under
// Claude Desktop's Application Support directory. The path includes
// session-specific UUIDs, so we search for the directory rather than
// constructing it.
func findCoworkPluginsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolving home directory: %w", err)
	}

	var baseDir string
	switch runtime.GOOS {
	case "darwin":
		baseDir = filepath.Join(home, "Library", "Application Support", "Claude", "local-agent-mode-sessions")
	case "linux":
		xdg := os.Getenv("XDG_CONFIG_HOME")
		if xdg == "" {
			xdg = filepath.Join(home, ".config")
		}
		baseDir = filepath.Join(xdg, "Claude", "local-agent-mode-sessions")
	default:
		return "", fmt.Errorf("unsupported platform")
	}

	// Search for the cowork_plugins directory. There should be exactly one
	// under the session hierarchy: <org-id>/<user-id>/cowork_plugins/
	var found string
	_ = filepath.WalkDir(baseDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip inaccessible directories
		}
		if d.IsDir() && d.Name() == "cowork_plugins" {
			found = path
			return filepath.SkipAll
		}
		// Don't descend into cowork_plugins subdirectories or session data.
		if d.IsDir() {
			// Allow descending into org/user UUID dirs (2 levels deep).
			depth := strings.Count(strings.TrimPrefix(path, baseDir), string(filepath.Separator))
			if depth > 2 {
				return filepath.SkipDir
			}
		}
		return nil
	})

	if found == "" {
		return "", fmt.Errorf("could not find cowork_plugins directory under %s", baseDir)
	}

	return found, nil
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
