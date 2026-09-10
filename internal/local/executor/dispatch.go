package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/clawvisor/clawvisor/internal/local/services"
)

// Dispatcher routes incoming requests to the appropriate executor.
type Dispatcher struct {
	registry      *services.Registry
	serverMgr     *ServerManager
	globalEnv     map[string]string
	maxOutputSize int64

	// Concurrency control.
	semaphore    chan struct{}
	queueTimeout time.Duration

	// Per-service concurrency. Without it, one chatty service can hold every
	// global slot and starve the others until they hit queueTimeout: a burst
	// of slow apple.imessage get_thread calls filled all 10 slots and made
	// concurrent apple.photos calls time out after 30s despite a healthy
	// tunnel. Capping each service below the global limit reserves headroom
	// for everyone else.
	//
	// Limitation: this bounds any *single* service, not the aggregate. With
	// the default of half the global limit, two simultaneously hot services
	// can still consume every slot and starve a third. Guaranteeing headroom
	// regardless of how many services are busy needs fair queueing or
	// reserved per-service slots rather than a static cap; deployments
	// running many active services should lower max_concurrent_per_service.
	perServiceMax int

	// perService is keyed by service ID and grows on first dispatch. Entries
	// are never evicted, which is deliberate: the key space is the set of
	// service IDs ever dispatched (a handful in practice, stable across
	// registry reloads) and each entry is an empty channel, so bounding it
	// against reloads would cost more than it saves.
	perServiceMu sync.Mutex
	perService   map[string]chan struct{}

	// runExec executes an exec-mode action. Swapped out in tests so dispatch
	// and concurrency behaviour can be exercised deterministically, without
	// depending on external binaries or subprocess timing.
	runExec func(
		ctx context.Context,
		svc *services.Service,
		action *services.Action,
		params map[string]string,
		globalEnv map[string]string,
		maxOutputSize int64,
		requestID string,
	) *ExecResult
}

// NewDispatcher creates a new request dispatcher. maxPerService caps how many
// requests a single service may run concurrently; values below 1 fall back to
// half the global limit so no service can monopolise the pool. Note this caps
// each service individually, not the aggregate — see the perServiceMax field
// comment for what that does and does not guarantee.
func NewDispatcher(
	registry *services.Registry,
	serverMgr *ServerManager,
	globalEnv map[string]string,
	maxOutputSize int64,
	maxConcurrent int,
	maxPerService int,
) *Dispatcher {
	if maxPerService < 1 {
		maxPerService = maxConcurrent / 2
	}
	// Always leave at least one slot usable, and never exceed the global cap.
	if maxPerService < 1 {
		maxPerService = 1
	}
	if maxPerService > maxConcurrent {
		maxPerService = maxConcurrent
	}

	return &Dispatcher{
		registry:      registry,
		serverMgr:     serverMgr,
		globalEnv:     globalEnv,
		maxOutputSize: maxOutputSize,
		semaphore:     make(chan struct{}, maxConcurrent),
		queueTimeout:  30 * time.Second,
		perServiceMax: maxPerService,
		perService:    make(map[string]chan struct{}),
		runExec:       RunExec,
	}
}

// serviceSemaphore returns the per-service slot channel, creating it on first
// use.
func (d *Dispatcher) serviceSemaphore(serviceID string) chan struct{} {
	d.perServiceMu.Lock()
	defer d.perServiceMu.Unlock()
	sem, ok := d.perService[serviceID]
	if !ok {
		sem = make(chan struct{}, d.perServiceMax)
		d.perService[serviceID] = sem
	}
	return sem
}

// Response is the generic response payload sent back to the cloud.
type Response struct {
	Success bool            `json:"success"`
	Data    json.RawMessage `json:"data,omitempty"`
	Error   string          `json:"error,omitempty"`
}

// Dispatch handles a request for a service action.
func (d *Dispatcher) Dispatch(ctx context.Context, serviceID, actionID string, params map[string]string, requestID string) *Response {
	svc, action := d.registry.GetAction(serviceID, actionID)
	if svc == nil {
		return &Response{Success: false, Error: fmt.Sprintf("unknown service: %s", serviceID)}
	}
	if action == nil {
		return &Response{Success: false, Error: fmt.Sprintf("unknown action: %s.%s", serviceID, actionID)}
	}

	// Validate required params.
	for _, p := range action.Params {
		if p.Required {
			if _, ok := params[p.Name]; !ok {
				return &Response{
					Success: false,
					Error:   fmt.Sprintf("missing required param: %s", p.Name),
				}
			}
		}
	}

	// Acquire dispatch slots with timeout. The semaphores themselves enforce
	// both concurrency and queue depth (buffered channel capacity).
	queueCtx, queueCancel := context.WithTimeout(ctx, d.queueTimeout)
	defer queueCancel()

	// Take the per-service slot first. Acquiring the global slot first would
	// let a service sit on scarce global capacity while blocked on its own
	// cap, which is the starvation this is meant to prevent.
	svcSem := d.serviceSemaphore(serviceID)
	select {
	case svcSem <- struct{}{}:
		defer func() { <-svcSem }()
	case <-ctx.Done():
		return &Response{Success: false, Error: "request discarded (connection closed)"}
	case <-queueCtx.Done():
		return &Response{
			Success: false,
			Error: fmt.Sprintf("timed out waiting for a dispatch slot for %s (limit %d concurrent)",
				serviceID, d.perServiceMax),
		}
	}

	select {
	case d.semaphore <- struct{}{}:
		defer func() { <-d.semaphore }()
	case <-ctx.Done():
		return &Response{Success: false, Error: "request discarded (connection closed)"}
	case <-queueCtx.Done():
		return &Response{Success: false, Error: "timed out waiting for dispatch slot"}
	}

	// Add request ID to env.
	globalEnv := make(map[string]string, len(d.globalEnv)+1)
	for k, v := range d.globalEnv {
		globalEnv[k] = v
	}

	// Dispatch based on service type.
	switch svc.Type {
	case "exec":
		result := d.runExec(ctx, svc, action, params, globalEnv, d.maxOutputSize, requestID)
		data, _ := json.Marshal(result.Data)
		return &Response{
			Success: result.Success,
			Data:    data,
			Error:   result.Error,
		}

	case "server":
		sp := d.serverMgr.Get(svc.ID)
		if sp == nil {
			return &Response{Success: false, Error: fmt.Sprintf("no server process for service: %s", serviceID)}
		}
		result := sp.Dispatch(ctx, action, params, d.maxOutputSize)
		data, _ := json.Marshal(result.Data)
		return &Response{
			Success: result.Success,
			Data:    data,
			Error:   result.Error,
		}

	default:
		return &Response{Success: false, Error: fmt.Sprintf("unknown service type: %s", svc.Type)}
	}
}
