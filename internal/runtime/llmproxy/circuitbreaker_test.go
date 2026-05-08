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

	// Probe still failing → re-open.
	_, err := cb.Verify(context.Background(), IntentVerifyRequest{})
	if !errors.Is(err, errors.Unwrap(err)) && err == nil {
		t.Fatalf("expected upstream error on probe")
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
