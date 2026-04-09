package notify

import "context"

// DecisionBus distributes callback decisions (Telegram inline buttons, push
// notification actions) across server instances. The in-memory implementation
// works for single-instance deployments; cloud deployments use a Redis-backed
// implementation for cross-instance delivery.
type DecisionBus interface {
	// Publish sends a decision to all subscribers.
	Publish(ctx context.Context, d CallbackDecision) error
	// Subscribe returns a channel that receives decisions from all instances.
	Subscribe(ctx context.Context) <-chan CallbackDecision
}

// LocalDecisionBus is a single-process DecisionBus backed by a Go channel.
type LocalDecisionBus struct {
	ch chan CallbackDecision
}

// NewLocalDecisionBus creates an in-memory decision bus.
func NewLocalDecisionBus() *LocalDecisionBus {
	return &LocalDecisionBus{ch: make(chan CallbackDecision, 64)}
}

// Publish sends a decision to the local channel.
func (b *LocalDecisionBus) Publish(_ context.Context, d CallbackDecision) error {
	select {
	case b.ch <- d:
	default:
		// Drop if full — same behavior as the existing channel-based flow.
	}
	return nil
}

// Subscribe returns the local channel.
func (b *LocalDecisionBus) Subscribe(_ context.Context) <-chan CallbackDecision {
	return b.ch
}
