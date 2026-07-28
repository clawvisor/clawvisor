package skills

import (
	"strings"
	"testing"
)

// TestRenderRejectsUnknownTarget guards the trap this package used to contain.
// dataForTarget's default arm returned a zero-value templateData, so
// RenderWithOptions(Target("codex"), …) SUCCEEDED and produced a document with
// no YAML frontmatter — which Codex rejects at startup — describing MCP tool
// names to a harness that has no MCP tools. It returned nil, so no caller could
// tell. Now an unknown target is an error, and the message names the valid set.
func TestRenderRejectsUnknownTarget(t *testing.T) {
	for _, bad := range []string{"", "codexx", "claude", "CLAUDE-CODE", "openclaw"} {
		if _, err := Render(Target(bad)); err == nil {
			t.Errorf("Render(%q) succeeded; want an error naming the valid targets", bad)
		}
	}
	if _, err := ParseTarget("nope"); err == nil {
		t.Error("ParseTarget(\"nope\") succeeded; want an error")
	}
	for _, name := range TargetNames() {
		if _, err := ParseTarget(name); err != nil {
			t.Errorf("ParseTarget(%q) = %v; every advertised target must parse", name, err)
		}
	}
}

// TestEveryTargetRendersFrontmatterExceptMCP — a skill loader needs YAML
// frontmatter to recognise the file at all, and Codex specifically refuses
// skills without name+description. MCP is the deliberate exception: its content
// is injected as initialize instructions, not read from disk as a skill.
func TestEveryTargetRendersFrontmatterExceptMCP(t *testing.T) {
	for _, target := range []Target{TargetClaudeCode, TargetCodex, TargetCowork} {
		got, err := Render(target)
		if err != nil {
			t.Fatalf("Render(%s): %v", target, err)
		}
		if !strings.HasPrefix(got, "---\n") {
			t.Errorf("%s: render must open with YAML frontmatter; got %.60q", target, got)
		}
		for _, field := range []string{"name: clawvisor", "description:"} {
			if !strings.Contains(got, field) {
				t.Errorf("%s: frontmatter missing %q", target, field)
			}
		}
	}

	mcp, err := Render(TargetMCP)
	if err != nil {
		t.Fatalf("Render(mcp): %v", err)
	}
	if strings.HasPrefix(mcp, "---\n") {
		t.Error("mcp render is injected as initialize instructions, not loaded from disk — frontmatter is unexpected here")
	}
}

// TestCodexRenderIsShellNotMCP — Codex executes shell, so it needs the concrete
// curl examples. Reusing the cowork/mcp branch would hand it MCP tool names it
// cannot call, and reusing the Claude Code branch verbatim would hand it a
// rationale about Claude Code's permission globs that is simply untrue of Codex.
func TestCodexRenderIsShellNotMCP(t *testing.T) {
	codex, err := Render(TargetCodex)
	if err != nil {
		t.Fatalf("Render(codex): %v", err)
	}

	// Shell, not MCP.
	if !strings.Contains(codex, "curl") {
		t.Error("codex render must contain concrete curl examples")
	}
	// The single-line-curl directive is shared and applies to any shell harness.
	if !strings.Contains(codex, "single line") {
		t.Error("codex render should keep the single-line-curl directive")
	}
	// …but not Claude Code's reason for it.
	if strings.Contains(codex, "separate approval prompt") {
		t.Error("codex render must not carry Claude Code's permission-glob rationale")
	}
	// OpenClaw's metadata block is meaningless to Codex and may trip its
	// stricter frontmatter parsing.
	if strings.Contains(codex, "openclaw") {
		t.Error("codex render must not carry the OpenClaw metadata block")
	}
}

// TestNonCurlTargetsGetNoCurlInstructions — the single-line-curl rule used to be
// gated on `not .Condensed` rather than .UseCurl, so cowork received an
// instruction about a mechanism it has no access to. Re-gating fixed that; this
// keeps it fixed.
func TestNonCurlTargetsGetNoCurlInstructions(t *testing.T) {
	for _, target := range []Target{TargetCowork, TargetMCP} {
		got, err := Render(target)
		if err != nil {
			t.Fatalf("Render(%s): %v", target, err)
		}
		if strings.Contains(got, "single line") {
			t.Errorf("%s uses MCP tools, not curl — it must not be told how to format curl commands", target)
		}
	}
}
