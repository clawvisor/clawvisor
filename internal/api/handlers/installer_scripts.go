package handlers

import (
	"embed"
	"fmt"
	"strings"
	"text/template"

	"github.com/clawvisor/clawvisor/pkg/version"
)

//go:embed installer_scripts/*.sh.tmpl
var installerScriptFS embed.FS

// installerScriptTmpl parses the three shell templates as one set so
// claude_code.sh.tmpl and codex.sh.tmpl can {{template "preamble" .}} into
// common.sh.tmpl. Parsed once at package init; renders are pure.
var installerScriptTmpl = template.Must(
	template.ParseFS(installerScriptFS, "installer_scripts/*.sh.tmpl"),
)

// installerScriptCtx is the rendering context for the per-harness shell
// installers. Mirrors installerCtx but only carries the fields the shell
// templates actually reference, plus a few precomputed strings (display
// label, codex provider slug, relay permission rule) so the templates stay
// flat — no Go-style helpers, just substitutions.
type installerScriptCtx struct {
	AppURL              string
	LLMURL              string
	Claim               string
	UserID              string
	AgentName           string
	Target              string
	HarnessLabel        string
	Slug                string
	DisplayLabel        string
	RelayPermissionRule string
}

// renderClaudeCodeShellInstaller renders the Claude Code shell installer
// from claude_code.sh.tmpl using the supplied installerCtx.
func renderClaudeCodeShellInstaller(ctx installerCtx) string {
	return renderInstallerScript("claude_code.sh.tmpl", installerScriptCtx{
		AppURL:              strings.TrimRight(ctx.AppURL, "/"),
		LLMURL:              strings.TrimRight(ctx.LLMURL, "/"),
		Claim:               ctx.Claim,
		UserID:              ctx.UserID,
		AgentName:           ctx.AgentName,
		Target:              string(InstallerClaudeCode),
		HarnessLabel:        "Claude Code",
		RelayPermissionRule: relayPermissionRule(),
	})
}

// renderCodexShellInstaller renders the Codex shell installer from
// codex.sh.tmpl. The provider slug is env-derived from the LLM proxy host so
// prod / staging / dev installs can coexist in one ~/.codex/config.toml.
func renderCodexShellInstaller(ctx installerCtx) string {
	slug, display := codexProviderID(ctx.LLMURL)
	return renderInstallerScript("codex.sh.tmpl", installerScriptCtx{
		AppURL:       strings.TrimRight(ctx.AppURL, "/"),
		LLMURL:       strings.TrimRight(ctx.LLMURL, "/"),
		Claim:        ctx.Claim,
		UserID:       ctx.UserID,
		AgentName:    ctx.AgentName,
		Target:       string(InstallerCodex),
		HarnessLabel: "Codex",
		Slug:         slug,
		DisplayLabel: display,
	})
}

func renderInstallerScript(name string, data installerScriptCtx) string {
	var b strings.Builder
	if err := installerScriptTmpl.ExecuteTemplate(&b, name, data); err != nil {
		// Template parsing happens at init; a runtime ExecuteTemplate failure
		// here would mean a template field is mis-typed, not a per-request
		// issue. Surface it as a comment in the script so the user sees the
		// failure mode instead of a silent empty response.
		return fmt.Sprintf("#!/bin/sh\necho 'clawvisor installer render failure: %s' >&2\nexit 1\n", err)
	}
	return b.String()
}

// relayPermissionRule returns the Claude Code Bash() permission rule for
// allowing curl to the public relay origin in the current build environment.
// Mirrors the rule shape used by addClaudePermissionRules in
// internal/daemon/claude_code.go.
func relayPermissionRule() string {
	host := "https://relay.clawvisor.com"
	if version.IsStaging() {
		host = "https://relay.staging.clawvisor.com"
	}
	return fmt.Sprintf("Bash(curl *%s/*)", host)
}
