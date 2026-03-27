package daemon

import (
	"fmt"

	"github.com/charmbracelet/huh"
)

// Integrate auto-detects installed coding agents and walks the user through
// setting up each one. Agents that require a running daemon (e.g. Claude
// Desktop MCP/OAuth) will fail gracefully if the daemon is not reachable.
func Integrate() error {
	dataDir, err := ensureDataDir()
	if err != nil {
		return err
	}

	agents := detectAgents()

	fmt.Println()
	fmt.Println(bold.Padding(0, 2).Render("Agent Integrations"))

	if len(agents) == 0 {
		fmt.Println(dim.Padding(0, 2).Render("No known agents detected on this machine."))
		fmt.Println()
		return nil
	}

	fmt.Println(dim.Padding(0, 2).Render("Detected the following agents:"))
	fmt.Println()
	for _, a := range agents {
		fmt.Println(green.Padding(0, 2).Render("  ✓ " + a.DisplayName))
	}
	fmt.Println()

	if hasClaudeCode(agents) {
		if err := IntegrateClaudeCode(); err != nil && err != huh.ErrUserAborted {
			fmt.Println(dim.Padding(0, 2).Render("  Warning: Claude Code integration failed: " + err.Error()))
		}
	}

	if hasClaudeDesktop(agents) {
		IntegrateClaudeDesktop()
		fmt.Println(dim.Padding(0, 4).Render("After authorizing Claude Desktop to connect with Clawvisor,"))
		fmt.Println(dim.Padding(0, 4).Render("you can ask your agent to perform tasks using any configured service."))
		fmt.Println()
	}

	// Print relay-based setup prompt for other agents.
	printAgentSetupPrompt(dataDir)

	return nil
}

// IntegrateClaudeCode installs the /clawvisor-setup slash command for Claude
// Code and optionally adds auto-approve rules for curl requests. Does not
// require a running daemon.
func IntegrateClaudeCode() error {
	dataDir, err := ensureDataDir()
	if err != nil {
		return err
	}

	return offerClaudeCodeSetup(dataDir)
}

// IntegrateClaudeDesktop configures the MCP connection for Claude Desktop.
// Requires a running daemon for OAuth registration on restart.
func IntegrateClaudeDesktop() {
	offerClaudeDesktopSetup()
}
