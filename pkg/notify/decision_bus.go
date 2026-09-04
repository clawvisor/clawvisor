package notify

import "context"

// DecisionBus distributes callback decisions (Telegram inline buttons, push
// notification actions) across server instances. The in-memory implementation
// works for single-instance deployments; cloud deployments use a Redis-backed
// implementation for cross-instance delivery.
type DecisionBus interface {
	// Publish sends a decision to all subscribers.
	Publish(ctx context.Context, d CallbackDecision) error
	// Subscribe returns a channel of deliveries. Each carries an Ack the
	// consumer MUST call once the decision has been fully handled.
	//
	// Delivery is at-least-once: a decision that is taken but never acked
	// is redelivered. Handlers therefore have to be idempotent — the
	// approval paths already are, since they CAS from "pending_approval"
	// and a duplicate simply loses the race.
	Subscribe(ctx context.Context) <-chan Delivery
}

// Delivery is one decision plus the acknowledgement that retires it.
//
// The ack exists because the previous design lost approvals: the queue read
// was destructive, so between taking a decision and finishing with it the
// only copy lived in memory, and anything that stopped the process in that
// window — a scale-down, a rolling deploy — dropped a human's decision with
// no trace. Three attempts to close that with shutdown ordering each moved
// the loss instead. Nothing leaves the queue permanently until Ack is
// called.
type Delivery struct {
	Decision CallbackDecision
	// Ack retires the decision. Not calling it means redelivery, which is
	// the desired outcome when handling failed or the process died.
	Ack func()
}

// LocalDecisionBus is a single-process DecisionBus backed by a Go channel.
//
// Acks are no-ops: with one process there is no redelivery to arrange, and a
// decision only ever exists in this process's memory anyway.
type LocalDecisionBus struct {
	ch chan Delivery
}

// NewLocalDecisionBus creates an in-memory decision bus.
func NewLocalDecisionBus() *LocalDecisionBus {
	// Unbuffered: a buffer here would hold decisions that vanish if the
	// process stops, which is the loss the Redis bus exists to prevent.
	return &LocalDecisionBus{ch: make(chan Delivery)}
}

// Publish sends a decision to the local channel.
func (b *LocalDecisionBus) Publish(ctx context.Context, d CallbackDecision) error {
	select {
	case b.ch <- Delivery{Decision: d, Ack: func() {}}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Subscribe returns the local channel.
func (b *LocalDecisionBus) Subscribe(_ context.Context) <-chan Delivery {
	return b.ch
}
