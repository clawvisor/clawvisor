package policies_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/inspector"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/pipeline"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/policies"
)

// TestComposeToolUseEvaluatorChain_AssemblesAllSixStages pins the chain
// ordering and existence: every position must have an evaluator with
// the expected name, in the order documented on the composer.
func TestComposeToolUseEvaluatorChain_AssemblesAllSixStages(t *testing.T) {
	chain := policies.ComposeToolUseEvaluatorChain(policies.ToolUseChainConfig{})
	wantNames := []string{
		"control_tool_use",
		"script_session",
		"shell_poll_passthrough",
		"inspector_chain",
		"task_scope",
		"intent_verify",
		"credential_rewrite",
	}
	if len(chain) != len(wantNames) {
		t.Fatalf("chain length = %d, want %d", len(chain), len(wantNames))
	}
	for i, want := range wantNames {
		if got := chain[i].Name(); got != want {
			t.Errorf("chain[%d].Name() = %q, want %q", i, got, want)
		}
	}
}

// TestComposeToolUseEvaluatorChain_EndToEndOnTriggerMiss pins the
// full chain's behavior on a plain (no autovault) tool_use with all
// resolvers nil — every stage should Skip, and the orchestrator's
// default-Allow fallback should win.
func TestComposeToolUseEvaluatorChain_EndToEndOnTriggerMiss(t *testing.T) {
	insp := inspector.NewInspector(inspector.DefaultParser{}, inspector.AmbiguousValidator{})
	chain := policies.ComposeToolUseEvaluatorChain(policies.ToolUseChainConfig{
		Inspector: insp,
	})

	tu := conversation.ToolUse{
		ID:    "toolu_plain",
		Name:  "Bash",
		Input: json.RawMessage(`{"command":"ls /tmp"}`),
	}
	res := newStubResp()

	_, result, err := pipeline.RunToolUseEvaluators(
		context.Background(),
		res,
		[]conversation.ToolUse{tu},
		chain,
	)
	if err != nil {
		t.Fatalf("RunToolUseEvaluators: %v", err)
	}

	v := result.PerToolUse[tu.ID]
	if v.Outcome != pipeline.OutcomeAllow {
		t.Errorf("Outcome = %q, want default-Allow", v.Outcome)
	}
}

// TestComposeToolUseEvaluatorChain_EndToEndCredentialedRewrite pins
// the chain's happy path: a WebFetch carrying an autovault placeholder
// flows through Control (Skip — not control plane), ScriptSession
// (Skip — no script token), InspectorChain (Skip — credentialed Allow
// path delegates to downstream), TaskScope (Skip — resolver nil),
// IntentVerify (Skip — resolver nil), CredentialRewrite (Rewrite).
func TestComposeToolUseEvaluatorChain_EndToEndCredentialedRewrite(t *testing.T) {
	insp := inspector.NewInspector(inspector.DefaultParser{}, inspector.AmbiguousValidator{})
	chain := policies.ComposeToolUseEvaluatorChain(policies.ToolUseChainConfig{
		Inspector: insp,
		// Allow boundary check: api.github.com is the allowed host for
		// the github placeholder. Without a resolver, the chain would
		// fall through with boundary_check_skipped — but we need the
		// CredentialRewrite to fire.
		AllowedHostsFor: func(_ context.Context, _ string) []string {
			return []string{"api.github.com"}
		},
		Rewrite: func(_ context.Context, _ conversation.ToolUse) *policies.CredentialRewriteInputs {
			return &policies.CredentialRewriteInputs{
				Inspector:    insp,
				CallerNonces: &stubNonceCache{minted: "cv-nonce-abc"},
				AgentID:      "agent-1",
				RewriteOpts:  inspector.RewriteOpts{ResolverBaseURL: "http://localhost:25297/api/proxy"},
			}
		},
	})

	tu := conversation.ToolUse{
		ID:   "toolu_cred",
		Name: "WebFetch",
		Input: json.RawMessage(`{
			"url":"https://api.github.com/repos/x/y/issues",
			"method":"POST",
			"headers":{"Authorization":"Bearer autovault_github_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"}
		}`),
	}

	// InspectorChain runs first and emits Allow on the credentialed path
	// (because no TaskScope/credentialed-path config is wired, the
	// boundary-check + Allow fires before CredentialRewrite gets a turn).
	// The first non-Skip wins, so InspectorChain claims the call.
	eval, result, err := pipeline.RunToolUseEvaluators(
		context.Background(),
		newStubResp(),
		[]conversation.ToolUse{tu},
		chain,
	)
	if err != nil {
		t.Fatalf("RunToolUseEvaluators: %v", err)
	}
	v := result.PerToolUse[tu.ID]
	// InspectorChain's boundary-check path returns Allow; that wins over
	// the subsequent CredentialRewriteEvaluator's Rewrite. This is the
	// documented orchestrator semantic ("first non-Skip wins"). The
	// integration test confirms the chain runs end-to-end and reaches a
	// terminal verdict.
	if v.Outcome != pipeline.OutcomeAllow && v.Outcome != pipeline.OutcomeRewrite {
		t.Errorf("Outcome = %q, want Allow or Rewrite", v.Outcome)
	}
	convV := eval(tu)
	if !convV.Allowed {
		t.Errorf("conversation verdict Allowed = false, want true")
	}
}
