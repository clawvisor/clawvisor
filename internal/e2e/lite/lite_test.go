package lite

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/clawvisor/clawvisor/internal/e2e/lite/drivers"
)

// scenarioDirs is the explicit list of scenarios under library/ that
// the runner exercises. Kept explicit (vs. globbing) so a half-written
// scenario can be skipped by removing one line.
var scenarioDirs = []string{
	"broad_single_scope",
	"non_default_cli",
	"pure_inspection",
	"scope_drift_followup",
	"pivot_mid_execution",
	"denied_then_explain",
	// "incremental_within_scope" — removed: agents consistently
	// create a new task per user message, even when the new ask
	// targets the same file with the same tool. Cross-request
	// iteration-within-scope is a behavior we can't engineer
	// without artificially broadening task purposes. The library
	// dir is left in place so the finding is discoverable; see
	// .context/eval_harness_proposal.md for the writeup.
	"bug_fix_workflow",
	"clarification_then_task",
	"cross_file_inspection",
	"credential_handle_discovery",
	"credential_handle_discovery_unaided",
	"credential_multi_service_at_scale",
	"no_invented_placeholder",
	"credential_not_needed_for_local",
}

// allDrivers is every CLI driver the harness knows about. Each is
// asked Available() at test start; absent CLIs are skipped per-driver
// rather than failing the whole run.
func allDrivers() []drivers.Driver {
	return []drivers.Driver{
		drivers.NewClaude(),
		drivers.NewCodex(),
	}
}

// TestLiteProxyScenarios is the entry point. Each scenario is run
// against every available driver as a sub-test, so a single test
// invocation produces a (scenario × driver) matrix.
func TestLiteProxyScenarios(t *testing.T) {
	anthropicKey := ResolveAnthropicKey()
	openaiKey := ResolveOpenAIKey()
	if anthropicKey == "" && openaiKey == "" {
		t.Skipf("set %s/%s and/or %s to run lite-proxy scenarios",
			EnvAnthropicKey, EnvAnthropicKeyLegacy, EnvOpenAIKey)
	}
	keys := Keys{Anthropic: anthropicKey, OpenAI: openaiKey}

	for _, scnName := range scenarioDirs {
		scnName := scnName
		t.Run(scnName, func(t *testing.T) {
			for _, drv := range allDrivers() {
				drv := drv
				t.Run(drv.Name(), func(t *testing.T) {
					t.Parallel()
					runScenarioAgainstDriver(t, scnName, drv, keys)
				})
			}
		})
	}
}

func runScenarioAgainstDriver(t *testing.T, scnDir string, drv drivers.Driver, keys Keys) {
	ok, why := drv.Available()
	if !ok {
		t.Skipf("driver %s unavailable: %s", drv.Name(), why)
	}
	// Drivers require their corresponding upstream key.
	switch drv.Name() {
	case "claude":
		if keys.Anthropic == "" {
			t.Skipf("claude driver needs %s / %s", EnvAnthropicKey, EnvAnthropicKeyLegacy)
		}
	case "codex":
		if keys.OpenAI == "" {
			t.Skipf("codex driver needs %s", EnvOpenAIKey)
		}
	}

	scn, err := LoadScenario(filepath.Join("library", scnDir))
	if err != nil {
		t.Fatalf("load scenario: %v", err)
	}

	h, err := Start(t, scn, keys)
	if err != nil {
		t.Fatalf("harness start: %v", err)
	}

	wall := time.Duration(scn.Budget.WallClockSeconds) * time.Second
	if wall <= 0 {
		wall = 5 * time.Minute
	}
	ctx, cancel := context.WithTimeout(context.Background(), wall)
	defer cancel()

	sess, err := drv.Start(ctx, drivers.Config{
		LiteProxyURL:    h.EndpointURL(),
		AgentToken:      h.AgentToken,
		Workspace:       h.Workspace,
		MaxTurnsPerStep: scn.Budget.MaxTurnsPerStep,
		Approver:        NewScriptedApprover(scn.Approvals),
		Logf:            t.Logf,
	})
	if err != nil {
		if drivers.IsSkip(err) {
			t.Skipf("driver %s skipped: %v", drv.Name(), err)
		}
		t.Fatalf("driver %s start: %v", drv.Name(), err)
	}
	defer sess.Close()

	var stepFailures []string
	for i, step := range scn.Script {
		t.Logf("=== step %d (%s) say: %s", i+1, drv.Name(), oneLine(step.Say, 200))
		out, err := sess.Send(ctx, step.Say)
		if err != nil {
			stepFailures = append(stepFailures, fmt.Sprintf("step %d: send error: %v", i+1, err))
			break
		}
		t.Logf("    duration=%dms toolCalls=%d taskApprove=%d taskDeny=%d toolBlocks=%d final=%q",
			out.DurationMs, out.ToolCallCount,
			out.TaskApprovalPromptsApproved, out.TaskApprovalPromptsDenied,
			out.ToolUseBlocksSeen, oneLine(out.FinalText, 200))
		for k := 0; k < out.ToolCallCount; k++ {
			h.Counters.Inc(SeriesToolCalls)
		}
		for k := 0; k < out.TaskApprovalPromptsApproved; k++ {
			h.Counters.Inc(SeriesApprovalsAllowSession)
		}
		for k := 0; k < out.TaskApprovalPromptsDenied; k++ {
			h.Counters.Inc(SeriesApprovalsDeny)
		}
		for k := 0; k < out.ToolUseBlocksSeen; k++ {
			h.Counters.Inc(SeriesToolUseBlock)
		}
		// Evaluate per-step filesystem expectations against the
		// workspace tempdir (ground truth, ignores what the agent said).
		fails := StepEval(ctx, h.Workspace, step.Expect)
		if len(fails) > 0 {
			stepFailures = append(stepFailures,
				fmt.Sprintf("step %d expectations: %s", i+1, strings.Join(fails, "; ")))
			break
		}
		// Enforce the scenario-level tool-call budget. The driver's
		// per-step ToolCallCount is summed via SeriesToolCalls; exceed
		// the cap and we fail the run early rather than letting the
		// agent burn budget across remaining steps.
		if limit := scn.Budget.MaxToolCallsTotal; limit > 0 && h.Counters.Get(SeriesToolCalls) > limit {
			stepFailures = append(stepFailures,
				fmt.Sprintf("step %d: tool_calls=%d exceeded MaxToolCallsTotal=%d",
					i+1, h.Counters.Get(SeriesToolCalls), limit))
			break
		}
	}

	snap := h.Counters.Snapshot()
	if active, err := h.CountActiveTasksForAgent(ctx); err == nil {
		snap["tasks.active"] = active
	}
	t.Logf("[%s/%s] counters: %v", scnDir, drv.Name(), snap)

	scenFails := ScenarioEval(scn.Expects.Hard, snap)

	if len(stepFailures) > 0 || len(scenFails) > 0 {
		t.Errorf("scenario %s / driver %s failed", scn.ID, drv.Name())
		for _, m := range stepFailures {
			t.Errorf("  [step] %s", m)
		}
		for _, m := range scenFails {
			t.Errorf("  [scenario] %s", m)
		}
	}
}

// Series names used by the harness counters. Kept here (vs. in
// agent_loop.go which is gone now) so callers don't have to chase
// strings across packages.
const (
	SeriesApprovalsAllowSession = "approvals.allow_session"
	SeriesApprovalsDeny         = "approvals.deny"
	SeriesToolUseBlock          = "lite_proxy.tool_use.block"
	SeriesToolCalls             = "tool_calls"
)

func oneLine(s string, max int) string {
	out := []rune{}
	for _, r := range s {
		if r == '\n' || r == '\r' {
			out = append(out, ' ')
			continue
		}
		out = append(out, r)
	}
	if len(out) <= max {
		return string(out)
	}
	return string(out[:max]) + "…"
}
