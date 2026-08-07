package executor

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/clawvisor/clawvisor/internal/local/services"
)

// testRegistry builds a registry with two exec services: a slow one and a fast
// one, each with a single action.
func testRegistry(t *testing.T, slowFor time.Duration) *services.Registry {
	t.Helper()

	mkService := func(id string, run []string) *services.Service {
		return &services.Service{
			ID:   id,
			Name: id,
			Type: "exec",
			Actions: []services.Action{{
				ID:      "go",
				Name:    "go",
				Run:     run,
				Timeout: 30 * time.Second,
			}},
		}
	}

	reg := services.NewRegistry()
	reg.Load(&services.DiscoverResult{Services: []*services.Service{
		mkService("chatty", []string{"sleep", formatSeconds(slowFor)}),
		mkService("quiet", []string{"true"}),
	}})
	return reg
}

func formatSeconds(d time.Duration) string {
	return time.Duration(d).Truncate(time.Millisecond).String()
}

// TestDispatchPerServiceFairness is the regression test for one service
// starving another. Production hit this when a burst of slow
// apple.imessage get_thread calls consumed every global dispatch slot and
// concurrent apple.photos calls sat until the 30s queue timeout even though
// the tunnel was healthy.
//
// Global limit 4, per-service limit 2: the chatty service can hold at most 2
// slots, so the quiet service must still get one promptly. Without the
// per-service cap, all 4 global slots go to chatty and the quiet request waits
// for a sleep to finish.
func TestDispatchPerServiceFairness(t *testing.T) {
	const (
		slowFor       = 2 * time.Second
		globalLimit   = 4
		perSvcLimit   = 2
		chattyBurst   = 8
		quietDeadline = 1500 * time.Millisecond
	)

	reg := testRegistry(t, slowFor)
	d := NewDispatcher(reg, nil, nil, 1<<20, globalLimit, perSvcLimit)

	ctx := context.Background()

	// Flood the dispatcher with slow requests from one service.
	var wg sync.WaitGroup
	for i := 0; i < chattyBurst; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d.Dispatch(ctx, "chatty", "go", nil, "req-chatty")
		}()
	}

	// Give the burst time to claim every slot it can.
	time.Sleep(300 * time.Millisecond)

	start := time.Now()
	resp := d.Dispatch(ctx, "quiet", "go", nil, "req-quiet")
	elapsed := time.Since(start)

	if !resp.Success {
		t.Fatalf("quiet request failed: %s", resp.Error)
	}
	if elapsed > quietDeadline {
		t.Fatalf("quiet request waited %v behind the chatty service; want under %v "+
			"(per-service cap should reserve global slots)", elapsed, quietDeadline)
	}

	wg.Wait()
}

// TestDispatchPerServiceCapDefaults checks the derivation and clamping of the
// per-service limit, since a wrong value here either disables the protection
// or throttles a lone service below the global limit.
func TestDispatchPerServiceCapDefaults(t *testing.T) {
	tests := []struct {
		name          string
		maxConcurrent int
		maxPerService int
		want          int
	}{
		{"derives half when unset", 10, 0, 5},
		{"derives half when negative", 10, -1, 5},
		{"never below one", 1, 0, 1},
		{"honours explicit value", 10, 3, 3},
		{"clamps above global limit", 4, 99, 4},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := NewDispatcher(services.NewRegistry(), nil, nil, 1<<20, tc.maxConcurrent, tc.maxPerService)
			if d.perServiceMax != tc.want {
				t.Fatalf("perServiceMax = %d, want %d", d.perServiceMax, tc.want)
			}
		})
	}
}

// TestDispatchServiceSemaphoreIsStable guards the lazy map: repeated lookups
// for a service must return the same channel, or the cap would not hold.
func TestDispatchServiceSemaphoreIsStable(t *testing.T) {
	d := NewDispatcher(services.NewRegistry(), nil, nil, 1<<20, 10, 2)

	first := d.serviceSemaphore("apple.photos")
	second := d.serviceSemaphore("apple.photos")
	if first != second {
		t.Fatal("serviceSemaphore returned a new channel for the same service")
	}
	if other := d.serviceSemaphore("apple.imessage"); other == first {
		t.Fatal("distinct services share a semaphore")
	}

	// Concurrent first-use must not race or produce distinct channels.
	var wg sync.WaitGroup
	got := make([]chan struct{}, 16)
	for i := range got {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			got[i] = d.serviceSemaphore("concurrent.svc")
		}(i)
	}
	wg.Wait()
	for i := range got {
		if got[i] != got[0] {
			t.Fatalf("goroutine %d observed a different semaphore", i)
		}
	}
}
