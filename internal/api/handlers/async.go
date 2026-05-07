package handlers

import (
	"context"
	"log/slog"
	"runtime/debug"
	"time"

	"github.com/clawvisor/clawvisor/internal/callback"
)

// safeGo runs fn in a new goroutine with panic recovery. Without this, a
// panic inside any of the many fire-and-forget goroutines spawned by the
// gateway/approvals/tasks handlers would crash the daemon and tear down
// every other in-flight request.
//
// name is included in the recovery log so the operator can locate the
// failing goroutine in code. Use a short stable string ("callback delivery",
// "chain extraction", etc.) — the stack trace itself is logged separately.
func safeGo(logger *slog.Logger, name string, fn func()) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				if logger != nil {
					logger.Error("goroutine panic recovered",
						"goroutine", name,
						"panic", r,
						"stack", string(debug.Stack()),
					)
				}
			}
		}()
		fn()
	}()
}

// CallbackDispatcher delivers callback.Payload values to agent callback URLs
// from a bounded pool of worker goroutines. The point is twofold:
//
//   - Cap concurrency so a slow / hung agent endpoint can't accumulate
//     thousands of goroutines while the daemon stays "up." Backpressure
//     drops the oldest semantics by refusing new work when full and
//     logging a warning.
//   - Provide one place to add panic recovery and structured failure
//     logging instead of repeating the pattern at ~16 call sites.
//
// Submit() never blocks the caller for more than the time it takes to
// acquire one queue slot; if the queue is full the callback is dropped and
// the loss is logged.
type CallbackDispatcher struct {
	queue  chan callbackJob
	logger *slog.Logger
	stop   chan struct{}
	done   chan struct{}
}

type callbackJob struct {
	url        string
	payload    *callback.Payload
	signingKey string
}

// NewCallbackDispatcher constructs a dispatcher with `workers` concurrent
// delivery goroutines and a queue depth of `queueSize`. Reasonable defaults
// for a single-host daemon are 16 workers / 1024 queue. Call Start to begin
// processing and Stop on shutdown.
func NewCallbackDispatcher(workers, queueSize int, logger *slog.Logger) *CallbackDispatcher {
	if workers < 1 {
		workers = 1
	}
	if queueSize < workers {
		queueSize = workers
	}
	return &CallbackDispatcher{
		queue:  make(chan callbackJob, queueSize),
		logger: logger,
		stop:   make(chan struct{}),
		done:   make(chan struct{}),
	}
}

// Start launches the worker goroutines. Each worker pulls from the queue
// and delivers the callback with a 30s timeout. Workers exit when the
// queue is closed by Stop.
func (d *CallbackDispatcher) Start(workers int) {
	if workers < 1 {
		workers = 1
	}
	finished := make(chan struct{}, workers)
	for i := 0; i < workers; i++ {
		safeGo(d.logger, "callback dispatcher worker", func() {
			defer func() { finished <- struct{}{} }()
			for job := range d.queue {
				cbCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				if err := callback.DeliverResult(cbCtx, job.url, job.payload, job.signingKey); err != nil && d.logger != nil {
					d.logger.Warn("callback delivery failed",
						"url", job.url,
						"request_id", job.payload.RequestID,
						"task_id", job.payload.TaskID,
						"err", err,
					)
				}
				cancel()
			}
		})
	}
	go func() {
		for i := 0; i < workers; i++ {
			<-finished
		}
		close(d.done)
	}()
}

// Submit enqueues a callback for delivery. Returns immediately; the caller
// never blocks for delivery. If the queue is full the callback is dropped
// and a warning is logged — backpressure is preferred over unbounded
// goroutine growth when the downstream is slow.
func (d *CallbackDispatcher) Submit(url string, payload *callback.Payload, signingKey string) {
	if d == nil || payload == nil || url == "" {
		return
	}
	job := callbackJob{url: url, payload: payload, signingKey: signingKey}
	select {
	case d.queue <- job:
	default:
		if d.logger != nil {
			d.logger.Warn("callback dispatcher queue full; dropping",
				"url", url,
				"request_id", payload.RequestID,
				"task_id", payload.TaskID,
			)
		}
	}
}

// Stop signals workers to drain and exit. Safe to call once; further
// Submit calls after Stop will block until queue space is available
// (none, since workers are exiting) and may panic if the queue is closed.
// Callers should ensure no Submits race a Stop.
func (d *CallbackDispatcher) Stop() {
	close(d.stop)
	close(d.queue)
	<-d.done
}
