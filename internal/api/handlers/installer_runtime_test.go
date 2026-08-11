package handlers

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestSkillOnlyInstallerRuntimeFreshTokenAndDryRun executes the rendered shell
// installers instead of only inspecting their text. Network and harness calls
// are replaced with deterministic fakes, but the installer itself runs under
// /bin/sh with a real HOME, jq transformations, token persistence, catalog
// preflight, harness invocation, verdict parsing, and final status banner.
//
// The fake catalog is intentionally local-only: it contains the normal
// "No cloud services" message followed by a local service heading. This pins
// the regression where the installer skipped a perfectly runnable dry run.
func TestSkillOnlyInstallerRuntimeFreshTokenAndDryRun(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skipf("no sh on PATH: %v", err)
	}
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skipf("no jq on PATH: %v", err)
	}

	h := NewInstallerHandler("", "", true, "", "https://app.example.com")
	for _, tc := range []struct {
		target string
		claim  string
	}{
		{target: "claude-code", claim: "ABCDEFGHIJ"},
		{target: "codex", claim: "CLAIMCODE0"},
	} {
		t.Run(tc.target, func(t *testing.T) {
			home := t.TempDir()
			fakeBin := filepath.Join(home, "fake-bin")
			if err := os.MkdirAll(fakeBin, 0o755); err != nil {
				t.Fatalf("mkdir fake bin: %v", err)
			}

			curlLog := filepath.Join(home, "curl.log")
			harnessLog := filepath.Join(home, "harness.log")
			writeExecutable(t, filepath.Join(fakeBin, "curl"), fakeInstallerCurl)
			writeExecutable(t, filepath.Join(fakeBin, installerHarnessForTarget(tc.target)), fakeInstallerHarness)

			// A valid old token in the same local slot used to bypass the connect
			// call entirely. Seed one and prove the installer replaces it.
			agentJSON := filepath.Join(home, ".clawvisor", "agents", "dev", tc.target+".json")
			if err := os.MkdirAll(filepath.Dir(agentJSON), 0o755); err != nil {
				t.Fatalf("mkdir agent dir: %v", err)
			}
			if err := os.WriteFile(agentJSON, []byte(`{"agent_id":"old-agent","token":"old-token"}`), 0o600); err != nil {
				t.Fatalf("seed old agent token: %v", err)
			}

			script := installerGetShell(t, h, tc.target, tc.claim)
			// Match the documented curl-pipe invocation, including the -s/--
			// boundary required to pass installer flags through to a stdin script.
			cmd := exec.Command("sh", "-s", "--", "--dry-run", "--no-allow-network")
			cmd.Stdin = strings.NewReader(script)
			cmd.Env = append(os.Environ(),
				"HOME="+home,
				"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
				"SHELL=/bin/false",
				"TERM=dumb",
				"CLAWVISOR_INVITE=",
				"CLAWVISOR_NO_TUI=1",
				"FAKE_CURL_LOG="+curlLog,
				"FAKE_HARNESS_LOG="+harnessLog,
			)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("rendered installer failed: %v\n%s", err, out)
			}
			if !strings.Contains(string(out), "Dry run PASSED") {
				t.Fatalf("installer did not report a passed dry run:\n%s", out)
			}
			if tc.target == "claude-code" {
				for _, want := range []string{
					"Initial prompt:\nUse the Clawvisor skill",
					"Claude:\n\tStarting the dry run.\n\tSelecting a service.",
					"\tSelecting a service.\n\nTool (Bash): curl https://app.example.com/api/skill/catalog",
					"Tool (Bash): curl https://app.example.com/api/skill/catalog\n\nTool result:\n\tcatalog fetched",
					"Tool result:\n\tcatalog fetched\n\tservice selected\n\nClaude:\n\tDry run results:",
					"\t- In-scope call: catalog read succeeded.\n\t- Out-of-scope call: write was held and did not execute.\n\nCLAWVISOR_DRY_RUN: PASS",
				} {
					if !strings.Contains(string(out), want) {
						t.Errorf("Claude transcript missing %q:\n%s", want, out)
					}
				}
			} else {
				for _, want := range []string{
					"Codex: Starting the dry run.",
					"Tool (exec): load Clawvisor skill",
					"Tool result: Clawvisor skill loaded",
					"Tool (exec): /bin/zsh -lc 'curl https://app.example.com/api/skill/catalog'",
					"Tool result:\n\tcatalog fetched\n\tservice selected",
					"Codex:\n\tDry run results:",
					"\t- In-scope call: catalog read succeeded.\n\t- Out-of-scope call: write was held and did not execute.\n\nCLAWVISOR_DRY_RUN: PASS",
					"CLAWVISOR_DRY_RUN: PASS pending_scope_expansion",
				} {
					if !strings.Contains(string(out), want) {
						t.Errorf("Codex transcript missing %q:\n%s", want, out)
					}
				}
				for _, noisy := range []string{
					"VERY NOISY SKILL BODY",
					"loading hooks from both ",
					"tokens used",
				} {
					if strings.Contains(string(out), noisy) {
						t.Errorf("Codex transcript retained noisy output %q:\n%s", noisy, out)
					}
				}
			}

			calls := readTestFile(t, curlLog)
			if !strings.Contains(calls, "/api/agents/connect?claim="+tc.claim) {
				t.Fatalf("fresh claim never reached connect endpoint; calls:\n%s", calls)
			}
			persisted := readTestFile(t, agentJSON)
			if !strings.Contains(persisted, `"token": "fresh-token"`) || strings.Contains(persisted, "old-token") {
				t.Fatalf("installer did not replace old local token with fresh token:\n%s", persisted)
			}

			harness := readTestFile(t, harnessLog)
			if !strings.Contains(harness, "CLAWVISOR_URL=https://app.example.com") ||
				!strings.Contains(harness, "CLAWVISOR_AGENT_TOKEN=fresh-token") {
				t.Fatalf("dry-run harness did not receive fresh Clawvisor env:\n%s", harness)
			}
			if tc.target == "codex" {
				for _, want := range []string{
					"--json --ephemeral --sandbox workspace-write",
					"sandbox_workspace_write.network_access=true",
				} {
					if !strings.Contains(harness, want) {
						t.Errorf("codex dry-run args missing %q:\n%s", want, harness)
					}
				}
			} else {
				if !strings.Contains(harness, "--permission-mode auto --verbose --output-format stream-json") {
					t.Errorf("Claude dry-run args missing auto permission mode:\n%s", harness)
				}
				if strings.Contains(harness, "dangerously-skip-permissions") {
					t.Errorf("Claude dry run bypassed permission checks:\n%s", harness)
				}
			}
		})
	}
}

func installerHarnessForTarget(target string) string {
	if target == "claude-code" {
		return "claude"
	}
	return "codex"
}

func writeExecutable(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatalf("write executable %s: %v", path, err)
	}
}

func readTestFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

const fakeInstallerCurl = `#!/bin/sh
printf '%s\n' "$*" >> "$FAKE_CURL_LOG"

out=''
want_out=no
for arg in "$@"; do
    if [ "$want_out" = yes ]; then
        out=$arg
        want_out=no
        continue
    fi
    case "$arg" in
        -o) want_out=yes ;;
        -o*) out=${arg#-o} ;;
    esac
done

case "$*" in
    *'/api/agents/connect?'*)
        body='{"status":"approved","token":"fresh-token","agent_id":"fresh-agent"}'
        ;;
    *'/api/skill/catalog'*)
        body='# Your Clawvisor Service Catalog

No cloud services are currently activated.

---

# Local Services

## File System
Service: local.files (via test-daemon)

- **read_file**(path) — Read a file'
        ;;
    *'/skill/SKILL.md'*)
        body='---
name: clawvisor
description: test skill
---'
        ;;
    *)
        body='{}'
        ;;
esac

if [ -n "$out" ]; then
    if [ "$out" != /dev/null ]; then
        printf '%s\n' "$body" > "$out"
    fi
else
    printf '%s\n' "$body"
fi
`

const fakeInstallerHarness = `#!/bin/sh
printf 'ARGS=%s\n' "$*" > "$FAKE_HARNESS_LOG"
printf 'CLAWVISOR_URL=%s\n' "$CLAWVISOR_URL" >> "$FAKE_HARNESS_LOG"
printf 'CLAWVISOR_AGENT_TOKEN=%s\n' "$CLAWVISOR_AGENT_TOKEN" >> "$FAKE_HARNESS_LOG"
case "$*" in
    *'--output-format stream-json'*)
        printf '%s\n' \
            '{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Starting the dry run.\nSelecting a service."}]}}' \
            '{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","name":"Bash","input":{"command":"curl https://app.example.com/api/skill/catalog"}}]}}' \
            '{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool-1","content":"catalog fetched\nservice selected"}]}}' \
            '{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Dry run results:\n- Service: local.files via test-daemon.\n- Approved scope: read_file.\n- In-scope call: catalog read succeeded.\n- Out-of-scope call: write was held and did not execute.\nCLAWVISOR_DRY_RUN: PASS pending_scope_expansion"}]}}'
        ;;
    *'--json'*)
        printf '%s\n' \
            '{"type":"thread.started","thread_id":"thread-1"}' \
            '{"type":"item.completed","item":{"id":"warning-1","type":"error","message":"loading hooks from both fake locations"}}' \
            '{"type":"turn.started"}' \
            '{"type":"item.completed","item":{"id":"message-1","type":"agent_message","text":"Starting the dry run."}}' \
            '{"type":"item.completed","item":{"id":"skill-1","type":"command_execution","command":"/bin/zsh -lc sed /tmp/.codex/skills/clawvisor/SKILL.md","aggregated_output":"VERY NOISY SKILL BODY","exit_code":0,"status":"completed"}}' \
            '{"type":"item.completed","item":{"id":"command-1","type":"command_execution","command":"/bin/zsh -lc '\''curl https://app.example.com/api/skill/catalog'\''","aggregated_output":"catalog fetched\nservice selected","exit_code":0,"status":"completed"}}' \
            '{"type":"item.completed","item":{"id":"message-2","type":"agent_message","text":"Dry run results:\n- Service: local.files via test-daemon.\n- Approved scope: read_file.\n- In-scope call: catalog read succeeded.\n- Out-of-scope call: write was held and did not execute.\nCLAWVISOR_DRY_RUN: PASS pending_scope_expansion"}}' \
            '{"type":"turn.completed","usage":{"input_tokens":12345,"output_tokens":123}}'
        ;;
    *)
        printf 'CLAWVISOR_DRY_RUN: PASS pending_scope_expansion\n'
        ;;
esac
`
