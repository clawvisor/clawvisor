package isolation

import (
	"context"
	"errors"
	"time"
)

// Cleanup releases all resources owned by the Handle: stops the holder
// container, removes the bridge network, and closes the forwarders.
// Idempotent and best-effort: errors are aggregated and returned.
func (h *Handle) Cleanup() error {
	if h == nil || !h.cleaned.CompareAndSwap(false, true) {
		return h.cleanupErr
	}
	h.cleanupOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		var errs []error
		if h.holder != nil && h.holder.ContainerID != "" {
			if err := StopContainer(ctx, h.plan.DockerBin, h.holder.ContainerID); err != nil {
				errs = append(errs, err)
			}
		}
		if h.proxyFwd != nil {
			_ = h.proxyFwd.Close()
		}
		if h.apiFwd != nil {
			_ = h.apiFwd.Close()
		}
		if h.testFwd != nil {
			_ = h.testFwd.Close()
		}
		if h.network != nil && h.network.Name != "" {
			if err := removeNetwork(ctx, h.plan.DockerBin, h.network.Name); err != nil {
				errs = append(errs, err)
			}
		}
		h.cleanupErr = errors.Join(errs...)
	})
	return h.cleanupErr
}
