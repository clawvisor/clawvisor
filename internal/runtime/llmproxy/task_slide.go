package llmproxy

import (
	"context"
	"time"

	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/tasklifetime"
	"github.com/clawvisor/clawvisor/pkg/store"
)

const SlidingTaskSlide = tasklifetime.SlidingTaskSlide

type taskExpirySetter interface {
	UpdateTaskExpiresAt(ctx context.Context, id string, expiresAt time.Time) error
}

func SlideTaskExpiry(ctx context.Context, st taskExpirySetter, task *store.Task, now time.Time) (time.Time, bool, error) {
	return tasklifetime.SlideTaskExpiry(ctx, st, task, now)
}
