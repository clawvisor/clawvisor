package intentverify

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	runtimedecision "github.com/clawvisor/clawvisor/pkg/runtime/decision"
)

// Verifier matches the LLM intent-verification contract without pulling
// provider dependencies into the proxy pipeline.
type Verifier interface {
	Verify(ctx context.Context, req Request) (*Verdict, error)
}

type Request struct {
	TaskPurpose string
	ExpectedUse string
	Service     string
	Action      string
	Params      map[string]any
	Reason      string
	TaskID      string
	Lenient     bool
}

type Verdict struct {
	Allow       bool
	Explanation string
}

type Decision struct {
	TaskID       string
	TaskPurpose  string
	ExpectedUse  string
	Verification string
	HasAction    bool
}

type ResolvedAction struct {
	ServiceID string
	ActionID  string
}

type IsCircuitOpenFunc func(error) bool

// DecisionVerifierFor wraps a verifier so runtimedecision.AuthorizationInput
// can consume it directly.
func DecisionVerifierFor(v Verifier) runtimedecision.IntentVerifier {
	return decisionIntentVerifier{inner: v}
}

type decisionIntentVerifier struct {
	inner Verifier
}

func (v decisionIntentVerifier) Verify(ctx context.Context, req runtimedecision.IntentVerifyRequest) (*runtimedecision.IntentVerdict, error) {
	if v.inner == nil {
		return nil, nil
	}
	verdict, err := v.inner.Verify(ctx, Request{
		TaskPurpose: req.TaskPurpose,
		ExpectedUse: req.ExpectedUse,
		Service:     req.Service,
		Action:      req.Action,
		Params:      req.Params,
		Reason:      req.Reason,
		TaskID:      req.TaskID,
		Lenient:     req.Lenient,
	})
	if err != nil || verdict == nil {
		return nil, err
	}
	return &runtimedecision.IntentVerdict{
		Allow:       verdict.Allow,
		Explanation: verdict.Explanation,
	}, nil
}

// Run performs the per-task-scope intent check after a task/action match.
// Returns (reason, ok); ok=false only when the verifier refuses or when
// isCircuitOpen classifies an error as fail-closed.
func Run(ctx context.Context, verifier Verifier, dec Decision, resolved ResolvedAction, tu conversation.ToolUse, isCircuitOpen IsCircuitOpenFunc) (string, bool) {
	if verifier == nil || !dec.HasAction {
		return "", true
	}
	mode := dec.Verification
	if mode == "off" {
		return "", true
	}
	var params map[string]any
	if len(tu.Input) > 0 {
		_ = json.Unmarshal(tu.Input, &params)
	}
	verdict, err := verifier.Verify(ctx, Request{
		TaskPurpose: dec.TaskPurpose,
		ExpectedUse: dec.ExpectedUse,
		Service:     resolved.ServiceID,
		Action:      resolved.ActionID,
		Params:      params,
		Reason:      "lite-proxy tool_use " + tu.Name,
		TaskID:      dec.TaskID,
		Lenient:     mode == "lenient",
	})
	if err != nil {
		if isCircuitOpen != nil && isCircuitOpen(err) {
			return "verifier_circuit_open", false
		}
		return fmt.Sprintf("verifier_error: %s", err.Error()), false
	}
	if verdict == nil {
		return "", true
	}
	if verdict.Allow {
		return verdict.Explanation, true
	}
	return verdict.Explanation, false
}
