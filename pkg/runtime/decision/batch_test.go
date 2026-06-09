package decision

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// blockingIntentVerifier holds Verify until release is closed so the
// test can observe concurrent goroutines parked inside the verifier
// (proof that fan-out is real and not serial).
type blockingIntentVerifier struct {
	release   chan struct{}
	concurrent int32
	peak       int32
	verdict    *IntentVerdict
}

func (b *blockingIntentVerifier) Verify(ctx context.Context, _ IntentVerifyRequest) (*IntentVerdict, error) {
	cur := atomic.AddInt32(&b.concurrent, 1)
	for {
		peak := atomic.LoadInt32(&b.peak)
		if cur <= peak || atomic.CompareAndSwapInt32(&b.peak, peak, cur) {
			break
		}
	}
	defer atomic.AddInt32(&b.concurrent, -1)
	select {
	case <-b.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return b.verdict, nil
}

func TestEvaluateAuthorizationBatch_ParallelizesVerifierCalls(t *testing.T) {
	agentID := "agent-1"
	verifier := &blockingIntentVerifier{
		release: make(chan struct{}),
		verdict: &IntentVerdict{Allow: true, Explanation: "fits task"},
	}
	task := taskWithExpectedTool("task-1", agentID, "exec_command", "read repo files")

	mkInput := func(id string) AuthorizationInput {
		return AuthorizationInput{
			ToolUse:        conversation.ToolUse{ID: id, Name: "exec_command", Input: []byte(`{"cmd":"cat README.md"}`)},
			AgentID:        agentID,
			CandidateTasks: []*store.Task{task},
			IntentVerifier: verifier,
		}
	}

	inputs := []AuthorizationInput{mkInput("toolu_1"), mkInput("toolu_2"), mkInput("toolu_3")}

	done := make(chan []AuthorizationOutcome, 1)
	go func() {
		done <- EvaluateAuthorizationBatch(context.Background(), inputs)
	}()

	// Wait until all three verifier calls are parked concurrently. If
	// fan-out regressed to serial, peak would never exceed 1 and this
	// loop would time out.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&verifier.peak) == 3 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if got := atomic.LoadInt32(&verifier.peak); got != 3 {
		close(verifier.release)
		<-done
		t.Fatalf("peak concurrent verifier calls = %d, want 3", got)
	}

	close(verifier.release)
	outcomes := <-done

	if len(outcomes) != 3 {
		t.Fatalf("outcomes len = %d, want 3", len(outcomes))
	}
	for i, o := range outcomes {
		if o.Err != nil {
			t.Fatalf("outcome[%d] err = %v, want nil", i, o.Err)
		}
		if o.Decision.Kind != VerdictAllow || o.Decision.Source != SourceTaskScope {
			t.Fatalf("outcome[%d] decision = %+v, want task-scope allow", i, o.Decision)
		}
	}
}

func TestEvaluateAuthorizationBatch_EmptyInputs(t *testing.T) {
	outcomes := EvaluateAuthorizationBatch(context.Background(), nil)
	if len(outcomes) != 0 {
		t.Fatalf("outcomes len = %d, want 0", len(outcomes))
	}
}

func TestEvaluateAuthorizationBatch_SingletonDoesNotSpawnGoroutine(t *testing.T) {
	// Smoke test the singleton fast-path: identical result shape to the
	// batched path. Concurrency itself isn't observable; we just check
	// the verdict matches a direct EvaluateAuthorization.
	agentID := "agent-1"
	toolAllow := rule("tool-allow", "tool", "allow", &agentID)
	toolAllow.ToolName = "Bash"
	input := AuthorizationInput{
		ToolUse:   toolUse("Bash", nil),
		AgentID:   agentID,
		ToolRules: []*store.RuntimePolicyRule{toolAllow},
	}

	outcomes := EvaluateAuthorizationBatch(context.Background(), []AuthorizationInput{input})
	if len(outcomes) != 1 {
		t.Fatalf("outcomes len = %d, want 1", len(outcomes))
	}
	if outcomes[0].Err != nil {
		t.Fatalf("outcome err = %v, want nil", outcomes[0].Err)
	}
	if outcomes[0].Decision.Kind != VerdictAllow || outcomes[0].Decision.Source != SourceRuleAllow {
		t.Fatalf("decision = %+v, want rule allow", outcomes[0].Decision)
	}
}

// erroringIntentVerifier returns an error for a specific tool_use ID
// so the test can verify that errors are isolated to their input.
type erroringIntentVerifier struct {
	failTaskID string
	err        error
	verdict    *IntentVerdict
}

func (e *erroringIntentVerifier) Verify(_ context.Context, req IntentVerifyRequest) (*IntentVerdict, error) {
	if req.TaskID == e.failTaskID {
		return nil, e.err
	}
	return e.verdict, nil
}

func TestEvaluateAuthorizationBatch_PerInputErrorIsolation(t *testing.T) {
	agentID := "agent-1"
	taskA := taskWithExpectedTool("task-A", agentID, "exec_command", "read repo files")
	taskB := taskWithExpectedTool("task-B", agentID, "exec_command", "read repo files")
	wantErr := errors.New("verifier exploded")
	verifier := &erroringIntentVerifier{
		failTaskID: "task-A",
		err:        wantErr,
		verdict:    &IntentVerdict{Allow: true, Explanation: "ok"},
	}

	inputs := []AuthorizationInput{
		{
			ToolUse:        conversation.ToolUse{ID: "toolu_A", Name: "exec_command", Input: []byte(`{"cmd":"cat README.md"}`)},
			AgentID:        agentID,
			CandidateTasks: []*store.Task{taskA},
			IntentVerifier: verifier,
		},
		{
			ToolUse:        conversation.ToolUse{ID: "toolu_B", Name: "exec_command", Input: []byte(`{"cmd":"cat README.md"}`)},
			AgentID:        agentID,
			CandidateTasks: []*store.Task{taskB},
			IntentVerifier: verifier,
		},
	}

	outcomes := EvaluateAuthorizationBatch(context.Background(), inputs)
	if len(outcomes) != 2 {
		t.Fatalf("outcomes len = %d, want 2", len(outcomes))
	}
	if !errors.Is(outcomes[0].Err, wantErr) {
		t.Fatalf("outcomes[0].Err = %v, want %v", outcomes[0].Err, wantErr)
	}
	if outcomes[1].Err != nil {
		t.Fatalf("outcomes[1].Err = %v, want nil (sibling shouldn't be poisoned)", outcomes[1].Err)
	}
	if outcomes[1].Decision.Kind != VerdictAllow {
		t.Fatalf("outcomes[1].Decision = %+v, want allow", outcomes[1].Decision)
	}
}
