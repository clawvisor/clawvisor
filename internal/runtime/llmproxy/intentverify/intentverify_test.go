package intentverify

import (
	"context"
	"errors"
	"testing"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
)

type stubVerifier struct {
	verdict *Verdict
	err     error
}

func (v stubVerifier) Verify(context.Context, Request) (*Verdict, error) {
	return v.verdict, v.err
}

func TestRunVerifierErrorFailsOpenUnlessCircuitOpen(t *testing.T) {
	dec := Decision{HasAction: true, Verification: "strict", TaskID: "task-1"}
	resolved := ResolvedAction{ServiceID: "local.files", ActionID: "read_file"}
	tu := conversation.ToolUse{ID: "toolu_1", Name: "Read", Input: []byte(`{"path":"/tmp/a"}`)}

	reason, ok := Run(context.Background(), stubVerifier{err: errors.New("verifier unavailable")}, dec, resolved, tu, nil)
	if !ok {
		t.Fatalf("transient verifier error should fail open")
	}
	if reason != "verifier_error" {
		t.Fatalf("reason = %q, want verifier_error", reason)
	}

	reason, ok = Run(context.Background(), stubVerifier{err: errors.New("circuit open")}, dec, resolved, tu, func(error) bool { return true })
	if ok {
		t.Fatalf("circuit-open verifier error should fail closed")
	}
	if reason != "verifier_circuit_open" {
		t.Fatalf("reason = %q", reason)
	}
}

func TestRunMalformedToolInputReturnsAuditBreadcrumb(t *testing.T) {
	dec := Decision{HasAction: true, Verification: "strict", TaskID: "task-1"}
	resolved := ResolvedAction{ServiceID: "local.files", ActionID: "read_file"}
	tu := conversation.ToolUse{ID: "toolu_1", Name: "Read", Input: []byte(`{not-json`)}

	reason, ok := Run(context.Background(), stubVerifier{verdict: &Verdict{Allow: true}}, dec, resolved, tu, nil)
	if !ok {
		t.Fatalf("allow verdict should stay allowed")
	}
	if reason != "params_parse_failed" {
		t.Fatalf("reason = %q, want params_parse_failed", reason)
	}
}
