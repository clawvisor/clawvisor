package llmproxy

import (
	"context"
	"errors"
	"testing"
	"time"
)

type flakyVerifier struct {
	err     error
	verdict *IntentVerdict
	calls   int
}

func (f *flakyVerifier) Verify(ctx context.Context, req IntentVerifyRequest) (*IntentVerdict, error) {
	f.calls++
	return f.verdict, f.err
}

func TestCircuitBreaker_TripsAfterThreshold(t *testing.T) {
	now := time.Unix(0, 0)
	clock := func() time.Time { return now }
	upstream := &flakyVerifier{err: errors.New("boom")}
	cb := NewCircuitBreakerVerifier(upstream, CircuitBreakerConfig{
		FailureThreshold: 3,
		CooldownDuration: 30 * time.Second,
		Now:              clock,
	})

	// First 3 errors pass through and trip the breaker.
	for i := 0; i < 3; i++ {
		_, err := cb.Verify(context.Background(), IntentVerifyRequest{})
		if err == nil || errors.Is(err, ErrCircuitOpen) {
			t.Fatalf("call %d: expected upstream error, got %v", i, err)
		}
	}
	// 4th call short-circuits.
	_, err := cb.Verify(context.Background(), IntentVerifyRequest{})
	if !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("expected ErrCircuitOpen, got %v", err)
	}
	if upstream.calls != 3 {
		t.Errorf("upstream called %d times, want 3", upstream.calls)
	}
	if cb.State() != "open" {
		t.Errorf("state=%s, want open", cb.State())
	}
}

func TestCircuitBreaker_ClosesOnHalfOpenSuccess(t *testing.T) {
	now := time.Unix(0, 0)
	clock := func() time.Time { return now }
	upstream := &flakyVerifier{err: errors.New("boom")}
	cb := NewCircuitBreakerVerifier(upstream, CircuitBreakerConfig{
		FailureThreshold: 2,
		CooldownDuration: 10 * time.Second,
		Now:              clock,
	})

	// Trip it.
	_, _ = cb.Verify(context.Background(), IntentVerifyRequest{})
	_, _ = cb.Verify(context.Background(), IntentVerifyRequest{})
	if cb.State() != "open" {
		t.Fatalf("expected open after 2 errors")
	}

	// Wait past cooldown.
	now = now.Add(11 * time.Second)
	if cb.State() != "half_open" {
		t.Fatalf("expected half_open after cooldown, got %s", cb.State())
	}

	// Half-open probe succeeds → circuit closes.
	upstream.err = nil
	upstream.verdict = &IntentVerdict{Allow: true}
	v, err := cb.Verify(context.Background(), IntentVerifyRequest{})
	if err != nil || !v.Allow {
		t.Fatalf("probe should succeed, got verdict=%v err=%v", v, err)
	}
	if cb.State() != "closed" {
		t.Errorf("state=%s after success, want closed", cb.State())
	}
}

func TestCircuitBreaker_ReopensOnHalfOpenFailure(t *testing.T) {
	now := time.Unix(0, 0)
	clock := func() time.Time { return now }
	upstream := &flakyVerifier{err: errors.New("boom")}
	cb := NewCircuitBreakerVerifier(upstream, CircuitBreakerConfig{
		FailureThreshold: 1,
		CooldownDuration: 10 * time.Second,
		Now:              clock,
	})

	_, _ = cb.Verify(context.Background(), IntentVerifyRequest{})
	if cb.State() != "open" {
		t.Fatalf("threshold=1 should trip immediately")
	}

	// Cool down.
	now = now.Add(11 * time.Second)
	if cb.State() != "half_open" {
		t.Fatalf("expected half_open")
	}

	// Probe still failing → re-open. We expect a non-nil upstream error
	// (not ErrCircuitOpen — half-open lets one probe through).
	_, err := cb.Verify(context.Background(), IntentVerifyRequest{})
	if err == nil {
		t.Fatalf("expected upstream error on probe, got nil")
	}
	if errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("probe should pass through to upstream, not short-circuit; got %v", err)
	}
	if cb.State() != "open" {
		t.Errorf("state=%s after probe failure, want open", cb.State())
	}

	// Subsequent calls during the new open window short-circuit.
	_, err = cb.Verify(context.Background(), IntentVerifyRequest{})
	if !errors.Is(err, ErrCircuitOpen) {
		t.Errorf("expected ErrCircuitOpen on follow-up, got %v", err)
	}
}

func TestCircuitBreaker_NilWrapper(t *testing.T) {
	cb := &CircuitBreakerVerifier{}
	v, err := cb.Verify(context.Background(), IntentVerifyRequest{})
	if v != nil || err != nil {
		t.Errorf("nil wrapper should be no-op, got v=%v err=%v", v, err)
	}
}

func TestCircuitBreaker_SuccessResetsConsecutiveErrors(t *testing.T) {
	now := time.Unix(0, 0)
	upstream := &flakyVerifier{}
	cb := NewCircuitBreakerVerifier(upstream, CircuitBreakerConfig{
		FailureThreshold: 3,
		CooldownDuration: 10 * time.Second,
		Now:              func() time.Time { return now },
	})

	upstream.err = errors.New("boom")
	_, _ = cb.Verify(context.Background(), IntentVerifyRequest{})
	_, _ = cb.Verify(context.Background(), IntentVerifyRequest{})
	// Now succeed once — counter resets.
	upstream.err = nil
	upstream.verdict = &IntentVerdict{Allow: true}
	_, _ = cb.Verify(context.Background(), IntentVerifyRequest{})
	if cb.State() != "closed" {
		t.Errorf("expected closed after success reset")
	}
	// 2 more failures should NOT trip (threshold=3, counter reset to 0).
	upstream.err = errors.New("boom")
	_, _ = cb.Verify(context.Background(), IntentVerifyRequest{})
	_, _ = cb.Verify(context.Background(), IntentVerifyRequest{})
	if cb.State() == "open" {
		t.Errorf("circuit should not be open — only 2 errors after reset")
	}
}

// With HalfOpenMaxCalls > 1, a stale success from one probe must not
// override another probe's failure during the same half-open burst.
// Before the fix, the first returning success closed the circuit and
// subsequent probe failures couldn't re-open it.
func TestCircuitBreaker_MultiProbeFailureWinsOverEarlierSuccess(t *testing.T) {
	now := time.Unix(0, 0)
	clock := func() time.Time { return now }
	// Switchable verifier — first call succeeds, second fails.
	v := &switchableVerifier{}
	cb := NewCircuitBreakerVerifier(v, CircuitBreakerConfig{
		FailureThreshold: 1,
		CooldownDuration: 10 * time.Second,
		HalfOpenMaxCalls: 2,
		Now:              clock,
	})

	// Trip the breaker.
	v.err = errors.New("boom")
	_, _ = cb.Verify(context.Background(), IntentVerifyRequest{})
	if cb.State() != "open" {
		t.Fatalf("expected open after first failure")
	}

	// Cool down → half-open.
	now = now.Add(11 * time.Second)
	if cb.State() != "half_open" {
		t.Fatalf("expected half_open, got %s", cb.State())
	}

	// Probe 1: succeeds. Should NOT close yet (need 2 successes).
	v.err = nil
	v.verdict = &IntentVerdict{Allow: true}
	if _, err := cb.Verify(context.Background(), IntentVerifyRequest{}); err != nil {
		t.Fatalf("probe 1 should succeed, got %v", err)
	}
	if cb.State() != "half_open" {
		t.Fatalf("after 1/2 successes circuit must stay half_open, got %s", cb.State())
	}

	// Probe 2: fails. Must re-open the circuit despite probe 1's success.
	v.err = errors.New("boom-again")
	if _, err := cb.Verify(context.Background(), IntentVerifyRequest{}); err == nil {
		t.Fatalf("probe 2 should propagate upstream failure")
	}
	if cb.State() != "open" {
		t.Fatalf("probe 2 failure must re-open the circuit, got %s", cb.State())
	}
}

type switchableVerifier struct {
	err     error
	verdict *IntentVerdict
}

func (s *switchableVerifier) Verify(ctx context.Context, req IntentVerifyRequest) (*IntentVerdict, error) {
	return s.verdict, s.err
}

// All-success probes must accumulate to HalfOpenMaxCalls before closing.
func TestCircuitBreaker_RequiresAllProbesToClose(t *testing.T) {
	now := time.Unix(0, 0)
	clock := func() time.Time { return now }
	v := &switchableVerifier{err: errors.New("boom")}
	cb := NewCircuitBreakerVerifier(v, CircuitBreakerConfig{
		FailureThreshold: 1,
		CooldownDuration: 10 * time.Second,
		HalfOpenMaxCalls: 3,
		Now:              clock,
	})
	_, _ = cb.Verify(context.Background(), IntentVerifyRequest{})  // trip
	now = now.Add(11 * time.Second)

	v.err = nil
	v.verdict = &IntentVerdict{Allow: true}
	for i := 0; i < 2; i++ {
		if _, err := cb.Verify(context.Background(), IntentVerifyRequest{}); err != nil {
			t.Fatalf("probe %d should succeed: %v", i+1, err)
		}
		if cb.State() != "half_open" {
			t.Fatalf("after %d/%d successes circuit must stay half_open, got %s",
				i+1, 3, cb.State())
		}
	}
	// Third success: closes.
	if _, err := cb.Verify(context.Background(), IntentVerifyRequest{}); err != nil {
		t.Fatalf("probe 3 should succeed: %v", err)
	}
	if cb.State() != "closed" {
		t.Fatalf("3/3 successes must close circuit, got %s", cb.State())
	}
}
