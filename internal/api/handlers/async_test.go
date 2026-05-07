package handlers

import (
	"io"
	"log/slog"
	"sync"
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
