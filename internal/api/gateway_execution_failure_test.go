package api_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/clawvisor/clawvisor/pkg/adapters"
)

func TestGatewayClassifiedAdapterFailuresSurviveDuplicateRequest(t *testing.T) {
	tests := []struct {
		name         string
		kind         adapters.ExecutionFailureKind
		wantCode     string
		wantOutcome  string
		wantTimedOut bool
	}{
		{
			name:        "definite",
			kind:        adapters.ExecutionFailureDefinite,
			wantCode:    "PROVIDER_DEFINITE_FAILURE",
			wantOutcome: "provider_definite_failure",
		},
		{
			name:        "stale version",
			kind:        adapters.ExecutionFailureStaleVersion,
			wantCode:    "STALE_VERSION",
			wantOutcome: "provider_stale_version",
		},
		{
			name:        "ambiguous",
			kind:        adapters.ExecutionFailureAmbiguous,
			wantCode:    "AMBIGUOUS_OUTCOME",
			wantOutcome: "provider_ambiguous",
		},
	}

	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			serviceID := fmt.Sprintf("mock.classified.%d", i)
			adapter := newMockAdapter(serviceID, "run").withError(&adapters.ExecutionFailure{
				Kind:     tc.kind,
				TimedOut: tc.wantTimedOut,
				Err:      errors.New("classified provider failure"),
			})
			env := newTestEnv(t, adapter)
			sc := newScenario(t, env, "calendar-agent")
			taskID := sc.createApprovedTask(t, env, serviceID, "run", true)
			requestID := fmt.Sprintf("classified-%d", i)

			first := sc.gatewayRequestWithTask(env, requestID, serviceID, "run", taskID)
			assertGatewayExecutionFailure(t, first, tc.wantCode, string(tc.kind), tc.wantTimedOut)

			duplicate := sc.gatewayRequestWithTask(env, requestID, serviceID, "run", taskID)
			assertGatewayExecutionFailure(t, duplicate, tc.wantCode, string(tc.kind), tc.wantTimedOut)
			if duplicate["deduped"] != true {
				t.Fatalf("duplicate response missing deduped=true: %v", duplicate)
			}
			if duplicate["audit_id"] != first["audit_id"] {
				t.Fatalf("duplicate audit_id = %v, want canonical %v", duplicate["audit_id"], first["audit_id"])
			}
			if adapter.executeCount() != 1 {
				t.Fatalf("adapter executions = %d, want 1", adapter.executeCount())
			}

			entry, err := env.Store.GetAuditEntryByRequestIDAndTask(context.Background(), requestID, sc.session.UserID, taskID)
			if err != nil {
				t.Fatalf("GetAuditEntryByRequestIDAndTask: %v", err)
			}
			if entry.Outcome != tc.wantOutcome {
				t.Fatalf("audit outcome = %q, want %q", entry.Outcome, tc.wantOutcome)
			}
		})
	}
}

func TestGatewayClickExecutionTimeoutAfterCommitRemainsIdempotent(t *testing.T) {
	var simulatedCommits atomic.Int32
	adapter := newMockAdapter("mock.click-timeout", "respond_to_event").
		withExecuteHook(func() { simulatedCommits.Add(1) }).
		withError(&adapters.ExecutionFailure{
			Kind:     adapters.ExecutionFailureAmbiguous,
			TimedOut: true,
			Err:      errors.New("provider timed out after accepting the mutation"),
		})
	env := newTestEnv(t, adapter)
	sc := newScenario(t, env, "calendar-agent")
	taskID := sc.createApprovedTask(t, env, "mock.click-timeout", "respond_to_event", false)
	requestID := "click-timeout-after-commit"

	pending := sc.gatewayRequestWithTask(env, requestID, "mock.click-timeout", "respond_to_event", taskID)
	if pending["status"] != "pending" {
		t.Fatalf("initial status = %v, want pending: %v", pending["status"], pending)
	}
	resp := sc.session.do("POST", fmt.Sprintf("/api/approvals/%s/approve", requestID), nil)
	mustStatus(t, resp, http.StatusOK)

	resp = env.do(
		"POST",
		fmt.Sprintf("/api/gateway/request/%s/execute?task_id=%s", requestID, taskID),
		sc.AgentToken,
		nil,
	)
	executed := mustStatus(t, resp, http.StatusOK)
	assertGatewayExecutionFailure(t, executed, "PROVIDER_TIMEOUT", "ambiguous", true)
	if simulatedCommits.Load() != 1 {
		t.Fatalf("simulated provider commits = %d, want 1", simulatedCommits.Load())
	}

	duplicate := sc.gatewayRequestWithTask(env, requestID, "mock.click-timeout", "respond_to_event", taskID)
	assertGatewayExecutionFailure(t, duplicate, "PROVIDER_TIMEOUT", "ambiguous", true)
	if duplicate["deduped"] != true {
		t.Fatalf("duplicate response missing deduped=true: %v", duplicate)
	}
	if simulatedCommits.Load() != 1 || adapter.executeCount() != 1 {
		t.Fatalf(
			"duplicate re-executed mutation: commits=%d executions=%d",
			simulatedCommits.Load(),
			adapter.executeCount(),
		)
	}

	entry, err := env.Store.GetAuditEntryByRequestIDAndTask(context.Background(), requestID, sc.session.UserID, taskID)
	if err != nil {
		t.Fatalf("GetAuditEntryByRequestIDAndTask: %v", err)
	}
	if entry.Outcome != "provider_timeout" {
		t.Fatalf("audit outcome = %q, want provider_timeout", entry.Outcome)
	}
}

func assertGatewayExecutionFailure(
	t *testing.T,
	body map[string]any,
	wantCode, wantKind string,
	wantTimedOut bool,
) {
	t.Helper()
	if body["status"] != "error" {
		t.Fatalf("status = %v, want error: %v", body["status"], body)
	}
	if body["code"] != wantCode {
		t.Fatalf("code = %v, want %s: %v", body["code"], wantCode, body)
	}
	if body["failure_kind"] != wantKind {
		t.Fatalf("failure_kind = %v, want %s: %v", body["failure_kind"], wantKind, body)
	}
	if wantTimedOut {
		if body["timed_out"] != true {
			t.Fatalf("timed_out = %v, want true: %v", body["timed_out"], body)
		}
	} else if _, exists := body["timed_out"]; exists {
		t.Fatalf("unexpected timed_out field: %v", body)
	}
}
