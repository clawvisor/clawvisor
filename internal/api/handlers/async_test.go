package handlers

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/clawvisor/clawvisor/internal/callback"
)

// TestCallbackDispatcher_ConcurrentSubmitDuringStop is the regression
// guard for the send-on-closed-channel panic. The previous implementation
// used `select { case <-queue: default: }` which protects against a full
// channel but NOT a closed one — Stop closed the queue, and any
// background goroutine still calling Submit (notifier consumer, expiry
// sweeper, etc.) would crash the daemon. This test fans out 200
// goroutines hammering Submit while Stop runs concurrently and asserts
// no panic.
func TestCallbackDispatcher_ConcurrentSubmitDuringStop(t *testing.T) {
	d := NewCallbackDispatcher(4, 16, slog.New(slog.NewTextHandler(io.Discard, nil)))
	d.Start(4)

	// A modest URL so DeliverResult has something to attempt; the worker
	// timeout will fail fast since the host doesn't exist.
	const url = "http://127.0.0.1:1/never"
	payload := &callback.Payload{Type: "request", RequestID: "r-x", Status: "executed"}

	var wg sync.WaitGroup
	const submitters = 200
	wg.Add(submitters)
	for i := 0; i < submitters; i++ {
		go func() {
			defer wg.Done()
			// A handful of Submits per goroutine to maximize race window.
			for j := 0; j < 50; j++ {
				d.Submit(url, payload, "")
			}
		}()
	}

	// Give submitters a moment to start, then stop concurrently.
	time.Sleep(2 * time.Millisecond)
	d.Stop()

	// All submitters must finish (and not panic, which would propagate).
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("submitters did not finish within 5s after Stop")
	}

	// A second Stop call must be safe (no double-close panic).
	d.Stop()

	// Submit after Stop must drop silently, not panic.
	d.Submit(url, payload, "")
}

// TestDeliverOne_RecoversFromPanic is the unit-level proof that
// deliverOne's defer'd recover actually catches a panic from
// callback.DeliverResult. A nil payload + an unreachable-but-valid URL
// makes DeliverResult's error-log branch dereference payload.Type and
// nil-deref panic. If recover were missing, this test would die
// runtime.gopanic and the test framework would mark it failed.
func TestDeliverOne_RecoversFromPanic(t *testing.T) {
	d := NewCallbackDispatcher(1, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	// Don't Start — we're calling deliverOne synchronously.
	d.deliverOne(callbackJob{url: "http://127.0.0.1:1/never", payload: nil})
	// If we got here the recover worked.
}

// TestCallbackDispatcher_WorkerSurvivesPanickingJob is the integration
// regression guard against the previous "wrap-the-whole-loop in safeGo"
// pattern, which permanently lost a worker after one panic. With
// per-iteration recovery, a panic in one job must not prevent the SAME
// worker from processing the next.
//
// We inject a nil payload directly into the worker's queue (bypassing
// Submit's nil guard) so callback.DeliverResult's `*payload` deref
// panics inside deliverOne. We then send a real job through Submit and
// assert the httptest server sees it within 2s — proof the worker
// survived the panic and went back to the for-range loop.
func TestCallbackDispatcher_WorkerSurvivesPanickingJob(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	d := NewCallbackDispatcher(1, 8, slog.New(slog.NewTextHandler(io.Discard, nil)))
	d.Start(1)
	defer d.Stop()

	// Bypass Submit's payload != nil guard by enqueueing directly. The
	// URL passes ValidateCallbackURL but the connection refusal triggers
	// callback.DeliverResult's error-log branch, which dereferences
	// payload.Type — nil-deref panics. The defer'd recover in
	// deliverOne must catch it.
	d.queue <- callbackJob{url: "http://127.0.0.1:1/never", payload: nil}

	// Now send a legitimate job through Submit. If the worker died from
	// the previous panic, this never gets delivered.
	d.Submit(srv.URL, &callback.Payload{Type: "request", RequestID: "real-job", Status: "executed"}, "")

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if hits.Load() >= 1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("worker stopped after panic — real job was not delivered within 2s")
}
