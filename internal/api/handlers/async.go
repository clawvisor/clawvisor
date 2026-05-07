package handlers

import (
	"context"
	"log/slog"
	"runtime/debug"
	"sync"
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
	// closeMu protects against the send-on-closed-channel panic during
	// shutdown. Submit takes the read-lock around the channel send; Stop
	// takes the write-lock to flip closed and close the channel atomically
	// w.r.t. concurrent Submits. Daemon shutdown ordering doesn't track
	// every long-lived goroutine that might call Submit (notifier consumer,
	// expiry sweeper, etc.), so the dispatcher has to be self-protective.
	closeMu sync.RWMutex
	closed  bool
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
//
// Recovery is per-iteration: a panic inside callback.DeliverResult is
// caught for that single job, the worker logs it, and the for-range loop
// continues on the next job. Wrapping the whole loop in safeGo would
// instead drop the worker permanently after one panic — repeated panics
// would silently shrink the pool to zero workers, and Submit would start
// dropping every subsequent callback.
func (d *CallbackDispatcher) Start(workers int) {
	if workers < 1 {
		workers = 1
	}
	finished := make(chan struct{}, workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer func() { finished <- struct{}{} }()
			for job := range d.queue {
				d.deliverOne(job)
			}
		}()
	}
	go func() {
		for i := 0; i < workers; i++ {
			<-finished
		}
		close(d.done)
	}()
}

// deliverOne invokes callback.DeliverResult for a single job with panic
// recovery scoped to this iteration. A panic here logs and returns; the
// worker survives and processes the next job.
func (d *CallbackDispatcher) deliverOne(job callbackJob) {
	defer func() {
		if r := recover(); r != nil && d.logger != nil {
			// Payload may be nil if the panic source was an upstream
			// programming error that bypassed Submit's guard — read
			// fields defensively so the recovery handler doesn't itself
			// panic and crash the worker.
			var requestID, taskID string
			if job.payload != nil {
				requestID = job.payload.RequestID
				taskID = job.payload.TaskID
			}
			d.logger.Error("callback delivery panicked",
				"url", job.url,
				"request_id", requestID,
				"task_id", taskID,
				"panic", r,
				"stack", string(debug.Stack()),
			)
		}
	}()
	cbCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := callback.DeliverResult(cbCtx, job.url, job.payload, job.signingKey); err != nil && d.logger != nil {
		d.logger.Warn("callback delivery failed",
			"url", job.url,
			"request_id", job.payload.RequestID,
			"task_id", job.payload.TaskID,
			"err", err,
		)
	}
}

// Submit enqueues a callback for delivery. Returns immediately; the caller
// never blocks for delivery. If the queue is full the callback is dropped
// and a warning is logged — backpressure is preferred over unbounded
// goroutine growth when the downstream is slow. Safe to call concurrently
// with Stop: the lock prevents the send-on-closed-channel panic.
func (d *CallbackDispatcher) Submit(url string, payload *callback.Payload, signingKey string) {
	if d == nil || payload == nil || url == "" {
		return
	}
	d.closeMu.RLock()
	defer d.closeMu.RUnlock()
	if d.closed {
		// Post-shutdown — drop silently rather than panic. Loss is
		// preferable to crashing the daemon during graceful shutdown
		// when a background goroutine submits one final callback.
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

// Stop signals workers to drain and exit. Safe to call once. Concurrent
// Submits during/after Stop are dropped silently rather than panicking on
// a closed channel.
func (d *CallbackDispatcher) Stop() {
	d.closeMu.Lock()
	if d.closed {
		d.closeMu.Unlock()
		return
	}
	d.closed = true
	close(d.queue)
	d.closeMu.Unlock()
	close(d.stop)
	<-d.done
}
