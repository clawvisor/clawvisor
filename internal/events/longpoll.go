package events

import (
	"context"
	"slices"
	"time"
)

// WaitFor subscribes to the event hub and re-fetches state each time a
// matching event fires. It returns as soon as fetch reports resolved=true,
// or when the timeout / context expires (returning the last fetched value).
//
// eventTypes filters which event types trigger a re-fetch; nil or empty
// means any event triggers a re-fetch.
func WaitFor[T any](
	ctx context.Context,
	hub EventHub,
	userID string,
	timeout time.Duration,
	eventTypes []string,
	fetch func(context.Context, *Event) (T, bool),
) T {
	ch, unsub := hub.Subscribe(userID)
	defer unsub()

	// Initial fetch after subscribe: if the state we're waiting on already
	// resolved between the caller's last check and our Subscribe call, we'd
	// otherwise block until timeout for an event that never arrives. The
	// subscribe-then-check-then-wait order ensures any post-subscribe event
	// still wakes us via ch even if the pre-subscribe one is missed.
	if v, done := fetch(ctx, nil); done {
		return v
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	// Periodic poll: safety net for events the hub drops under buffer
	// overflow (Publish skips full per-subscriber buffers). The
	// initial-fetch above already covers publishes that landed between
	// the caller's last check and our Subscribe call, so this only fires
	// for the rare drop case. One second balances responsiveness against
	// load (one fetch/sec per waiter) — under normal conditions
	// nothing waits long enough for it to fire at all.
	poll := time.NewTicker(1 * time.Second)
	defer poll.Stop()

	for {
		select {
		case <-ctx.Done():
			v, _ := fetch(context.Background(), nil)
			return v
		case <-timer.C:
			v, _ := fetch(context.Background(), nil)
			return v
		case <-poll.C:
			if v, done := fetch(ctx, nil); done {
				return v
			}
		case evt, ok := <-ch:
			if !ok {
				v, _ := fetch(context.Background(), nil)
				return v
			}
			if len(eventTypes) > 0 && !slices.Contains(eventTypes, evt.Type) {
				continue
			}
			v, done := fetch(ctx, &evt)
			if done {
				return v
			}
		}
	}
}
