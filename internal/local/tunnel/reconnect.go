package tunnel

import (
	"math"
	"time"
)

// Backoff implements exponential backoff for reconnection.
type Backoff struct {
	attempt   int
	baseDelay time.Duration
	maxDelay  time.Duration
}

// NewBackoff creates a new backoff calculator.
func NewBackoff() *Backoff {
	return &Backoff{
		baseDelay: 1 * time.Second,
		maxDelay:  60 * time.Second,
	}
}

// Next returns the next backoff duration and increments the attempt counter.
func (b *Backoff) Next() time.Duration {
	delay := time.Duration(float64(b.baseDelay) * math.Pow(2, float64(b.attempt)))
	if delay > b.maxDelay {
		delay = b.maxDelay
	}
	b.attempt++
	return delay
}

// Reset resets the backoff counter after a successful connection.
func (b *Backoff) Reset() {
	b.attempt = 0
}
