package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/clawvisor/clawvisor/internal/local/services"
)

// Dispatcher routes incoming requests to the appropriate executor.
type Dispatcher struct {
	registry       *services.Registry
	serverMgr      *ServerManager
	globalEnv      map[string]string
	maxOutputSize  int64

	// Concurrency control.
	semaphore    chan struct{}
	queueTimeout time.Duration
}

// NewDispatcher creates a new request dispatcher.
func NewDispatcher(
	registry *services.Registry,
	serverMgr *ServerManager,
	globalEnv map[string]string,
	maxOutputSize int64,
	maxConcurrent int,
) *Dispatcher {
	return &Dispatcher{
		registry:      registry,
		serverMgr:     serverMgr,
		globalEnv:     globalEnv,
		maxOutputSize: maxOutputSize,
		semaphore:     make(chan struct{}, maxConcurrent),
		queueTimeout:  30 * time.Second,
	}
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

	// Acquire dispatch slot with timeout. The semaphore itself enforces both
	// concurrency and queue depth (buffered channel capacity).
	queueCtx, queueCancel := context.WithTimeout(ctx, d.queueTimeout)
	defer queueCancel()

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
		result := RunExec(ctx, svc, action, params, globalEnv, d.maxOutputSize, requestID)
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
