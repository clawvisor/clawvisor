package llmproxy

import (
	"context"
	"errors"
	"sync"
	"time"
)

// ErrCircuitOpen is the error returned by CircuitBreakerVerifier.Verify
// when the breaker is tripped. Callers should treat this as a verifier
// outage and decide whether to fail-open or fail-closed; postprocess
// fails closed (refuses the tool_use) when the breaker is open, so the
// system degrades to "no tool use possible" rather than "tool use with
// no LLM oversight".
var ErrCircuitOpen = errors.New("llmproxy: intent verifier circuit breaker open")

// CircuitBreakerConfig tunes the breaker. Production defaults via
// DefaultCircuitBreakerConfig: trip after 5 consecutive errors, stay
// open for 30s, then enter half-open and let one probe through.
type CircuitBreakerConfig struct {
	// FailureThreshold is the number of consecutive errors that trip
	// the breaker. Below the threshold, errors pass through. Default 5.
	FailureThreshold int

	// CooldownDuration is how long the breaker stays open before
	// transitioning to half-open. Default 30s.
	CooldownDuration time.Duration

	// HalfOpenMaxCalls is the number of probe calls allowed through
	// while the breaker is half-open. The first success closes; any
	// failure re-opens. Default 1.
	HalfOpenMaxCalls int

	// Now is the clock function. Defaults to time.Now; tests inject
	// a fake to control transitions deterministically.
	Now func() time.Time
}

// DefaultCircuitBreakerConfig returns production-tuned defaults.
func DefaultCircuitBreakerConfig() CircuitBreakerConfig {
	return CircuitBreakerConfig{
		FailureThreshold: 5,
		CooldownDuration: 30 * time.Second,
		HalfOpenMaxCalls: 1,
		Now:              time.Now,
	}
}

const (
	circuitClosed = iota
	circuitOpen
	circuitHalfOpen
)

// CircuitBreakerVerifier wraps an IntentVerifier and trips its circuit
// after consecutive errors, returning ErrCircuitOpen until cooldown.
// Closed → repeated errors trip to Open. Open → after cooldown, one
// half-open probe is allowed; success closes, failure re-opens.
type CircuitBreakerVerifier struct {
	inner IntentVerifier
	cfg   CircuitBreakerConfig

	mu                sync.Mutex
	state             int
	consecutiveErrors int
	openedAt          time.Time
	halfOpenInflight  int
	halfOpenSuccesses int
}

// NewCircuitBreakerVerifier wraps inner with a circuit breaker. Pass
// CircuitBreakerConfig{} to use defaults.
func NewCircuitBreakerVerifier(inner IntentVerifier, cfg CircuitBreakerConfig) *CircuitBreakerVerifier {
	if cfg.FailureThreshold <= 0 {
		cfg.FailureThreshold = DefaultCircuitBreakerConfig().FailureThreshold
	}
	if cfg.CooldownDuration <= 0 {
		cfg.CooldownDuration = DefaultCircuitBreakerConfig().CooldownDuration
	}
	if cfg.HalfOpenMaxCalls <= 0 {
		cfg.HalfOpenMaxCalls = 1
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &CircuitBreakerVerifier{inner: inner, cfg: cfg, state: circuitClosed}
}

// Verify is the IntentVerifier entry point. Returns ErrCircuitOpen when
// tripped; otherwise delegates to the wrapped verifier and updates the
// circuit state based on success/failure.
func (c *CircuitBreakerVerifier) Verify(ctx context.Context, req IntentVerifyRequest) (*IntentVerdict, error) {
	if c == nil || c.inner == nil {
		return nil, nil
	}
	if err := c.preCall(); err != nil {
		return nil, err
	}
	verdict, err := c.inner.Verify(ctx, req)
	c.postCall(err)
	return verdict, err
}

// State returns the current circuit state for tests + observability.
// Values: "closed", "open", "half_open".
func (c *CircuitBreakerVerifier) State() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.maybeTransitionToHalfOpenLocked()
	switch c.state {
	case circuitOpen:
		return "open"
	case circuitHalfOpen:
		return "half_open"
	}
	return "closed"
}

func (c *CircuitBreakerVerifier) preCall() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.maybeTransitionToHalfOpenLocked()
	switch c.state {
	case circuitOpen:
		return ErrCircuitOpen
	case circuitHalfOpen:
		if c.halfOpenInflight >= c.cfg.HalfOpenMaxCalls {
			return ErrCircuitOpen
		}
		c.halfOpenInflight++
	}
	return nil
}

func (c *CircuitBreakerVerifier) postCall(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	wasHalfOpen := c.state == circuitHalfOpen
	if wasHalfOpen {
		c.halfOpenInflight--
		if c.halfOpenInflight < 0 {
			c.halfOpenInflight = 0
		}
	}
	if err == nil {
		if wasHalfOpen {
			// Require HalfOpenMaxCalls consecutive probe successes before
			// closing. With max=1 (the default) this closes immediately
			// like before. With max>1 a single stale success can't override
			// other in-flight probe failures: the circuit stays half-open
			// until enough probes succeed, and any failure re-opens.
			c.halfOpenSuccesses++
			if c.halfOpenSuccesses >= c.cfg.HalfOpenMaxCalls {
				c.state = circuitClosed
				c.consecutiveErrors = 0
				c.halfOpenSuccesses = 0
				c.halfOpenInflight = 0
			}
			return
		}
		// Success in closed state: reset failure counter.
		c.state = circuitClosed
		c.consecutiveErrors = 0
		return
	}
	c.consecutiveErrors++
	if wasHalfOpen {
		// Any failure during the half-open burst trips the circuit again,
		// regardless of other in-flight successes. Reset success count so
		// the next half-open burst starts fresh.
		c.state = circuitOpen
		c.openedAt = c.cfg.Now()
		c.halfOpenSuccesses = 0
		c.halfOpenInflight = 0
		return
	}
	if c.consecutiveErrors >= c.cfg.FailureThreshold {
		c.state = circuitOpen
		c.openedAt = c.cfg.Now()
	}
}

func (c *CircuitBreakerVerifier) maybeTransitionToHalfOpenLocked() {
	if c.state != circuitOpen {
		return
	}
	if c.cfg.Now().Sub(c.openedAt) >= c.cfg.CooldownDuration {
		c.state = circuitHalfOpen
		c.halfOpenInflight = 0
		c.halfOpenSuccesses = 0
	}
}
