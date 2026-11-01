package executor

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/clawvisor/clawvisor/internal/local/services"
)

// testRegistry builds a registry of exec services, each with a single "go"
// action. The Run argv is never executed — tests inject a fake runExec — but
// it must be non-empty for the manifest to be well formed.
func testRegistry(t *testing.T, ids ...string) *services.Registry {
	t.Helper()

	svcs := make([]*services.Service, 0, len(ids))
	for _, id := range ids {
		svcs = append(svcs, &services.Service{
			ID:   id,
			Name: id,
			Type: "exec",
			Actions: []services.Action{{
				ID:      "go",
				Name:    "go",
				Run:     []string{"true"},
				Timeout: 30 * time.Second,
			}},
		})
	}

	reg := services.NewRegistry()
	reg.Load(&services.DiscoverResult{Services: svcs})
	return reg
}

// blockingExec returns a fake runExec that parks every request for the named
// service until release is closed, and completes all others immediately. Each
// parked start is signalled on the returned channel, so tests can synchronise
// on real dispatch progress instead of sleeping.
func blockingExec(blockService string, release <-chan struct{}) (
	func(context.Context, *services.Service, *services.Action, map[string]string, map[string]string, int64, string) *ExecResult,
	<-chan struct{},
) {
	started := make(chan struct{}, 64)

	fn := func(
		ctx context.Context,
		svc *services.Service,
		_ *services.Action,
		_ map[string]string,
		_ map[string]string,
		_ int64,
		_ string,
	) *ExecResult {
		if svc.ID != blockService {
			return &ExecResult{Success: true}
		}
		started <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
		}
		return &ExecResult{Success: true}
	}

	return fn, started
}

// TestDispatchPerServiceFairness is the regression test for one service
// starving another. Production hit this when a burst of slow
// apple.imessage get_thread calls consumed every global dispatch slot and
// concurrent apple.photos calls sat until the 30s queue timeout even though
// the tunnel was healthy.
//
// Global limit 4, per-service limit 2. The assertions are that exactly
// perSvcLimit chatty requests get to run — no more — and that the quiet
// service still gets a slot while chatty is saturated. Without the cap,
// chatty takes all 4 global slots and both assertions fail.
//
// The fake executor parks requests on a channel rather than shelling out, so
// the test cannot pass vacuously if an external binary is missing, and does
// not depend on subprocess timing.
func TestDispatchPerServiceFairness(t *testing.T) {
	const (
		globalLimit = 4
		perSvcLimit = 2
		chattyBurst = 8
	)

	release := make(chan struct{})
	fake, started := blockingExec("chatty", release)

	d := NewDispatcher(testRegistry(t, "chatty", "quiet"), nil, nil, 1<<20, globalLimit, perSvcLimit)
	d.runExec = fake

	ctx := context.Background()

	var wg sync.WaitGroup
	for i := 0; i < chattyBurst; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d.Dispatch(ctx, "chatty", "go", nil, "req-chatty")
		}()
	}
	defer func() {
		close(release)
		wg.Wait()
	}()

	// Wait for chatty to saturate its allowance. Failing here means the fake
	// executor was never reached, which would otherwise let the test pass
	// without exercising the cap at all.
	for i := 0; i < perSvcLimit; i++ {
		select {
		case <-started:
		case <-time.After(10 * time.Second):
			t.Fatalf("only %d of %d chatty requests started; executor never ran", i, perSvcLimit)
		}
	}

	// The cap must hold: no further chatty request may enter the executor
	// while the first perSvcLimit are still parked.
	select {
	case <-started:
		t.Fatalf("more than %d chatty requests ran concurrently; per-service cap not enforced", perSvcLimit)
	case <-time.After(500 * time.Millisecond):
	}

	// And the whole point: another service still gets through. The context
	// bound converts a starved dispatch into a failed response rather than a
	// 30s hang.
	quietCtx, quietCancel := context.WithTimeout(ctx, 5*time.Second)
	defer quietCancel()

	resp := d.Dispatch(quietCtx, "quiet", "go", nil, "req-quiet")
	if !resp.Success {
		t.Fatalf("quiet request starved by chatty service: %s", resp.Error)
	}
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

// TestDispatchSlotsAreReleased checks slots are returned after a request
// completes, so a burst does not permanently consume the pool.
func TestDispatchSlotsAreReleased(t *testing.T) {
	release := make(chan struct{})
	close(release) // never block
	fake, _ := blockingExec("none", release)

	d := NewDispatcher(testRegistry(t, "svc"), nil, nil, 1<<20, 2, 1)
	d.runExec = fake

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// More requests than the per-service cap, run sequentially: each must
	// succeed, which is only possible if slots are released.
	for i := 0; i < 5; i++ {
		if resp := d.Dispatch(ctx, "svc", "go", nil, "req"); !resp.Success {
			t.Fatalf("request %d failed: %s", i, resp.Error)
		}
	}
}
