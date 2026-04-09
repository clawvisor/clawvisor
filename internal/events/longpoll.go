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
	fetch func(context.Context) (T, bool),
) T {
	ch, unsub := hub.Subscribe(userID)
	defer unsub()

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			v, _ := fetch(context.Background())
			return v
		case <-timer.C:
			v, _ := fetch(context.Background())
			return v
		case evt, ok := <-ch:
			if !ok {
				v, _ := fetch(context.Background())
				return v
			}
			if len(eventTypes) > 0 && !slices.Contains(eventTypes, evt.Type) {
				continue
			}
			v, done := fetch(ctx)
			if done {
				return v
			}
		}
	}
}
