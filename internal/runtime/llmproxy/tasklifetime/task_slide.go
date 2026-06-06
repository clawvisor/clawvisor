package tasklifetime

import (
	"context"
	"time"

	"github.com/clawvisor/clawvisor/pkg/store"
)

// SlidingTaskSlide is how far forward each authorized tool_use bumps
// a sliding-lifetime task's expires_at. The slide is "as long as it's
// active": every successful scope+intent check on a sliding task moves
// the deadline to max(current, now+SlidingTaskSlide). No ceiling — the
// task expires only when the agent goes idle long enough for the
// window to lapse between calls. Sliding is an explicit opt-in
// lifetime; session tasks have a fixed deadline and don't slide.
const SlidingTaskSlide = 10 * time.Minute

// taskExpirySetter is the narrow subset of store.Store the slide path
// uses. Declared here so unit tests can stub the slide without
// implementing the full Store interface.
type taskExpirySetter interface {
	UpdateTaskExpiresAt(ctx context.Context, id string, expiresAt time.Time) error
}

// SlideTaskExpiry bumps the matched task's expires_at when the task
// opted into "sliding" lifetime and the slide would actually move the
// deadline forward. Returns (newExpiry, true, nil) on a write,
// (_, false, nil) when the slide was a no-op (non-sliding lifetime,
// nil expiry, or current expiry already past now+slide), and
// (_, false, err) if the store rejected the update.
//
// Callers should treat store errors as non-blocking: the slide is a
// UX affordance, not an authorization gate. The scope check that
// produced `task` already passed; failing the tool_use because we
// couldn't extend its TTL would be a worse user experience than
// silently letting the original deadline stand.
func SlideTaskExpiry(ctx context.Context, st taskExpirySetter, task *store.Task, now time.Time) (time.Time, bool, error) {
	if st == nil || task == nil || task.Lifetime != "sliding" || task.ExpiresAt == nil {
		return time.Time{}, false, nil
	}
	candidate := now.Add(SlidingTaskSlide)
	if !candidate.After(*task.ExpiresAt) {
		return *task.ExpiresAt, false, nil
	}
	if err := st.UpdateTaskExpiresAt(ctx, task.ID, candidate); err != nil {
		return time.Time{}, false, err
	}
	return candidate, true, nil
}
